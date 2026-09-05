package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/modify"
	"github.com/github/gh-stack/internal/stack"
	"github.com/spf13/cobra"
)

type rebaseOptions struct {
	branch                    string
	downstack                 bool
	upstack                   bool
	cont                      bool
	abort                     bool
	noTrunk                   bool
	remote                    string
	committerDateIsAuthorDate bool
}

type rebaseState struct {
	CurrentBranchIndex int      `json:"currentBranchIndex"`
	ConflictBranch     string   `json:"conflictBranch"`
	// ConflictWorktree is the worktree the interrupted rebase is running in,
	// empty when it is the worktree the rebase was started from. Rebase state
	// is private to a worktree, so --continue and --abort must be routed back
	// to it no matter where they are run from.
	ConflictWorktree          string            `json:"conflictWorktree,omitempty"`
	RemainingBranches         []string          `json:"remainingBranches"`
	OriginalBranch            string            `json:"originalBranch"`
	OriginalRefs              map[string]string `json:"originalRefs"`
	UseOnto                   bool              `json:"useOnto,omitempty"`
	OntoOldBase               string            `json:"ontoOldBase,omitempty"`
	CommitterDateIsAuthorDate bool              `json:"committerDateIsAuthorDate,omitempty"`
	NoTrunk                   bool              `json:"noTrunk,omitempty"`
	TrunkRef                  string            `json:"trunkRef,omitempty"`
	TrunkSHA                  string            `json:"trunkSha,omitempty"`
	StartIndex                int               `json:"startIndex,omitempty"`
	EndIndex                  int               `json:"endIndex,omitempty"`
}

const rebaseStateFile = "gh-stack-rebase-state"

func RebaseCmd(cfg *config.Config) *cobra.Command {
	opts := &rebaseOptions{}

	cmd := &cobra.Command{
		Use:   "rebase [branch]",
		Short: "Rebase a stack of branches",
		Long: `Pull from remote and do a cascading rebase across the stack.

Ensures that each branch in the stack has the tip of the previous
layer in its commit history, rebasing if necessary.

Use --no-trunk to skip fetching and rebasing with the trunk branch.
Only the inter-branch rebases are performed (branch 2 onto branch 1,
branch 3 onto branch 2, etc.).`,
		Example: `  # Rebase the entire stack
  $ gh stack rebase

  # Only rebase from trunk to the current branch
  $ gh stack rebase --downstack

  # Only rebase from current branch to the top
  $ gh stack rebase --upstack

  # Rebase stack branches without pulling from or rebasing with trunk
  $ gh stack rebase --no-trunk

  # Continue after resolving conflicts
  $ gh stack rebase --continue

  # Abort and restore all branches
  $ gh stack rebase --abort`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.branch = args[0]
			}
			return runRebase(cfg, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.downstack, "downstack", false, "Only rebase branches from trunk to current branch")
	cmd.Flags().BoolVar(&opts.upstack, "upstack", false, "Only rebase branches from current branch to top")
	cmd.Flags().BoolVar(&opts.noTrunk, "no-trunk", false, "Skip trunk — only rebase stack branches onto each other")
	cmd.Flags().BoolVar(&opts.cont, "continue", false, "Continue rebase after resolving conflicts")
	cmd.Flags().BoolVar(&opts.abort, "abort", false, "Abort rebase and restore all branches")
	cmd.Flags().StringVar(&opts.remote, "remote", "", "Remote to fetch from (defaults to auto-detected remote)")
	cmd.Flags().BoolVar(&opts.committerDateIsAuthorDate, "committer-date-is-author-date", false, "Set the committer date to the author date during rebase")
	cmd.Flags().BoolVar(&opts.committerDateIsAuthorDate, "preserve-dates", false, "Alias for --committer-date-is-author-date")

	return cmd
}

func runRebase(cfg *config.Config, opts *rebaseOptions) error {
	gitDir, err := stackDir(cfg)
	if err != nil {
		cfg.Errorf("not a git repository")
		return ErrNotInStack
	}

	if opts.cont {
		return continueRebase(cfg, gitDir)
	}

	if opts.abort {
		return abortRebase(cfg, gitDir)
	}

	if err := modify.CheckStateGuard(gitDir); err != nil {
		cfg.Errorf("%s", err)
		return ErrModifyRecovery
	}

	result, err := loadStack(cfg, opts.branch)
	if err != nil {
		return ErrNotInStack
	}
	sf := result.StackFile
	s := result.Stack
	currentBranch := result.CurrentBranch

	// Stack branches other worktrees have checked out are rebased inside those
	// worktrees; git will not rewrite them from here.
	worktrees := newWorktreeIndex(currentBranch)

	// Enable git rerere so conflict resolutions are remembered.
	if err := ensureRerere(cfg); errors.Is(err, errInterrupt) {
		return ErrSilent
	}

	var trunk trunkTarget
	if !opts.noTrunk {
		// Resolve remote for fetch and trunk comparison
		remote, err := pickRemote(cfg, currentBranch, opts.remote)
		if err != nil {
			if !errors.Is(err, errInterrupt) {
				cfg.Errorf("%s", err)
			}
			return ErrSilent
		}

		trunk, err = resolveTrunkTarget(cfg, s, remote, currentBranch, worktrees)
		if err != nil {
			return err
		}

		// Fast-forward stack branches that are behind their remote tracking branch.
		if err := git.FetchBranches(remote, activeBranchNames(s)); err != nil {
			cfg.Errorf("failed to fetch stack branches from %s: %v", remote, err)
			return ErrSilent
		}
		fastForwardBranches(cfg, s, remote, currentBranch, worktrees)
	}

	cfg.Printf("Stack detected: %s", s.DisplayChain())

	currentIdx := s.IndexOf(currentBranch)
	if currentIdx < 0 {
		currentIdx = 0
	}

	if opts.upstack && currentIdx >= 0 && s.Branches[currentIdx].IsMerged() {
		cfg.Warningf("Current branch %q has already been merged", currentBranch)
	}

	startIdx := 0
	endIdx := len(s.Branches)

	if opts.downstack {
		endIdx = currentIdx + 1
	}
	if opts.upstack {
		startIdx = currentIdx
	}

	// With --no-trunk, skip the first branch (which would rebase onto trunk).
	if opts.noTrunk && startIdx < 1 {
		startIdx = 1
	}

	branchesToRebase := s.Branches[startIdx:endIdx]

	if len(branchesToRebase) == 0 {
		cfg.Printf("No branches to rebase")
		return nil
	}

	cfg.Printf("Rebasing branches in order, starting from %s to %s",
		branchesToRebase[0].Branch, branchesToRebase[len(branchesToRebase)-1].Branch)

	// Sync PR state before rebase so we can detect merged PRs.
	_ = syncStackPRs(cfg, s)

	// Stop before the first rewrite if a worktree holding one of these branches
	// is not in a state where git can rebase it.
	rebaseNames := make([]string, 0, len(branchesToRebase))
	for _, br := range branchesToRebase {
		if !br.IsSkipped() {
			rebaseNames = append(rebaseNames, br.Branch)
		}
	}
	if err := checkWorktreesReady(cfg, worktrees, rebaseNames); err != nil {
		return err
	}

	originalRefs, err := resolveOriginalRefs(s)
	if err != nil {
		return fmt.Errorf("resolving branch refs: %w", err)
	}

	// Get --onto state from a merged branch immediately below the rebase range.
	// Ensures that when --upstack excludes merged branches, we still check the
	// immediate predecessor and use --onto if needed.
	needsOnto := false
	var ontoOldBase string
	if startIdx > 0 {
		prev := s.Branches[startIdx-1]
		if prev.IsMerged() {
			if sha, ok := originalRefs[prev.Branch]; ok {
				needsOnto = true
				ontoOldBase = sha
			}
		}
	}

	rebaseResult := cascadeRebase(cascadeRebaseOpts{
		Cfg:                       cfg,
		Stack:                     s,
		Branches:                  branchesToRebase,
		StartAbsIdx:               startIdx,
		OriginalRefs:              originalRefs,
		NeedsOnto:                 needsOnto,
		OntoOldBase:               ontoOldBase,
		CommitterDateIsAuthorDate: opts.committerDateIsAuthorDate,
		TrunkRef:                  trunk.Ref,
		Worktrees:                 worktrees,
	})

	if rebaseResult.Err != nil {
		cfg.Errorf("%v", rebaseResult.Err)
		if rebaseResult.Rebased {
			restoreRebaseRefs(cfg, currentBranch, originalRefs, worktrees)
		} else {
			_ = git.CheckoutBranch(currentBranch)
		}
		return ErrSilent
	}

	if rebaseResult.Conflicted {
		cfg.Warningf("Rebasing %s onto %s%s — conflict", rebaseResult.ConflictBranch, rebaseResult.ConflictBase,
			worktreeSuffix(rebaseResult.ConflictDir))

		state := &rebaseState{
			CurrentBranchIndex:        rebaseResult.ConflictIdx,
			ConflictBranch:            rebaseResult.ConflictBranch,
			ConflictWorktree:          rebaseResult.ConflictDir,
			RemainingBranches:         rebaseResult.Remaining,
			OriginalBranch:            currentBranch,
			OriginalRefs:              originalRefs,
			UseOnto:                   rebaseResult.NeedsOnto,
			OntoOldBase:               rebaseResult.OntoOldBase,
			CommitterDateIsAuthorDate: opts.committerDateIsAuthorDate,
			NoTrunk:                   opts.noTrunk,
			TrunkRef:                  trunk.Ref,
			TrunkSHA:                  trunk.SHA,
			StartIndex:                startIdx,
			EndIndex:                  endIdx,
		}
		if err := saveRebaseState(gitDir, state); err != nil {
			cfg.Warningf("failed to save rebase state: %s", err)
		}

		printConflictDetails(cfg, rebaseResult.ConflictBase, rebaseResult.ConflictDir)
		cfg.Printf("")

		reportConflictLocation(cfg, rebaseResult.ConflictBranch, rebaseResult.ConflictDir)
		cfg.Printf("Or abort this operation with `%s`",
			cfg.ColorCyan("gh stack rebase --abort"))
		return ErrConflict
	}

	_ = git.CheckoutBranch(currentBranch)

	if unstacked := verifyStacked(s, trunk.Ref, startIdx, endIdx); len(unstacked) > 0 {
		reportUnstacked(cfg, trunk.Ref, unstacked)
		if rebaseResult.Rebased {
			restoreRebaseRefs(cfg, currentBranch, originalRefs, worktrees)
		}
		return ErrSilent
	}

	updateBaseSHAs(s)

	_ = syncStackPRs(cfg, s)

	stack.SaveNonBlocking(gitDir, sf)

	merged := s.MergedBranches()
	if len(merged) > 0 {
		names := make([]string, len(merged))
		for i, m := range merged {
			names[i] = m.Branch
		}
		cfg.Printf("Skipped %d merged %s: %s", len(merged), plural(len(merged), "branch", "branches"), strings.Join(names, ", "))
	}

	rangeDesc := "All branches in stack"
	if opts.downstack {
		rangeDesc = fmt.Sprintf("All downstack branches up to %s", currentBranch)
	} else if opts.upstack {
		rangeDesc = fmt.Sprintf("All upstack branches from %s", currentBranch)
	}

	if opts.noTrunk {
		cfg.Printf("%s rebased locally (without trunk)", rangeDesc)
	} else {
		cfg.Printf("%s rebased locally with %s", rangeDesc, trunk.Describe())
	}
	cfg.Printf("To push up your changes, run `%s`",
		cfg.ColorCyan("gh stack push"))

	return nil
}

func continueRebase(cfg *config.Config, gitDir string) error {
	state, err := loadRebaseState(gitDir)
	if err != nil {
		cfg.Errorf("no rebase in progress")
		return ErrSilent
	}

	sf, err := stack.Load(gitDir)
	if err != nil {
		cfg.Errorf("failed to load stack state: %s", err)
		return ErrNotInStack
	}

	// Use the saved original branch to find the stack, since git may be in
	// a detached HEAD state during an active rebase.
	s, err := resolveStack(sf, state.OriginalBranch, cfg)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("no stack found for branch %s", state.OriginalBranch)
	}
	trunkRef := state.TrunkRef
	if trunkRef == "" {
		trunkRef = s.Trunk.Branch
	}
	trunkBase := state.TrunkSHA
	if trunkBase == "" {
		trunkBase = trunkRef
	}

	// Refresh PR state before selecting the base and cascading the remaining
	// branches. The queued flag is transient (not persisted), so it was lost
	// when the stack was reloaded from disk above. Without this, a queued
	// branch in the remaining cascade would be treated as active and its
	// frozen merge-queue branch would be rebased. Mirrors the syncStackPRs
	// call in runRebase before its cascade.
	_ = syncStackPRs(cfg, s)

	// The branch that had the conflict is stored in state; fall back to
	// looking it up by index for backwards compatibility with older state files.
	conflictBranch := state.ConflictBranch
	if conflictBranch == "" && state.CurrentBranchIndex >= 0 && state.CurrentBranchIndex < len(s.Branches) {
		conflictBranch = s.Branches[state.CurrentBranchIndex].Branch
	}

	cfg.Printf("Continuing rebase of stack, resuming from %s to %s%s",
		conflictBranch, s.Branches[len(s.Branches)-1].Branch, worktreeSuffix(state.ConflictWorktree))

	// The interrupted rebase lives in the worktree that owns the conflicting
	// branch, which is not necessarily this one.
	if git.IsRebaseInProgressIn(state.ConflictWorktree) {
		rebaseOpts := git.RebaseOpts{CommitterDateIsAuthorDate: state.CommitterDateIsAuthorDate}
		if err := git.RebaseContinueIn(state.ConflictWorktree, rebaseOpts); err != nil {
			return fmt.Errorf("rebase continue failed — resolve remaining conflicts%s and try again: %w",
				worktreeSuffix(state.ConflictWorktree), err)
		}
	}

	// Rebuild the worktree map for the remaining cascade. The conflicting
	// branch is no longer detached now that its rebase finished.
	currentBranch, _ := git.CurrentBranch()
	worktrees := newWorktreeIndex(currentBranch)

	var baseBranch string
	if state.UseOnto {
		// The --onto path targets the first non-merged ancestor, or trunk.
		baseBranch = trunkRef
		for j := state.CurrentBranchIndex - 1; j >= 0; j-- {
			if !s.Branches[j].IsMerged() {
				baseBranch = s.Branches[j].Branch
				break
			}
		}
	} else if state.CurrentBranchIndex > 0 {
		baseBranch = s.Branches[state.CurrentBranchIndex-1].Branch
	} else {
		baseBranch = trunkRef
	}
	cfg.Successf("Rebased %s onto %s%s", conflictBranch, baseBranch, worktreeSuffix(state.ConflictWorktree))

	// Rebase remaining branches using the shared cascade helper.
	if len(state.RemainingBranches) > 0 {
		// Validate all remaining branches still exist in the stack,
		// are in contiguous ascending order, and build the BranchRef slice.
		remainingRefs := make([]stack.BranchRef, 0, len(state.RemainingBranches))
		startAbsIdx := -1
		for i, name := range state.RemainingBranches {
			idx := s.IndexOf(name)
			if idx < 0 {
				return fmt.Errorf("branch %q from saved rebase state is no longer in the stack — the stack may have been modified since the rebase started; consider aborting with --abort", name)
			}
			if startAbsIdx < 0 {
				startAbsIdx = idx
			} else if idx != startAbsIdx+i {
				return fmt.Errorf("branch %q is at stack index %d, expected %d — the stack may have been reordered since the rebase started; consider aborting with --abort", name, idx, startAbsIdx+i)
			}
			remainingRefs = append(remainingRefs, s.Branches[idx])
		}

		result := cascadeRebase(cascadeRebaseOpts{
			Cfg:                       cfg,
			Stack:                     s,
			Branches:                  remainingRefs,
			StartAbsIdx:               startAbsIdx,
			OriginalRefs:              state.OriginalRefs,
			NeedsOnto:                 state.UseOnto,
			OntoOldBase:               state.OntoOldBase,
			CommitterDateIsAuthorDate: state.CommitterDateIsAuthorDate,
			TrunkRef:                  trunkBase,
			Worktrees:                 worktrees,
		})

		if result.Err != nil {
			cfg.Errorf("%v", result.Err)
			restoreRebaseRefs(cfg, state.OriginalBranch, state.OriginalRefs, worktrees)
			clearRebaseState(gitDir)
			return ErrSilent
		}

		if result.Conflicted {
			cfg.Warningf("Rebasing %s onto %s%s — conflict", result.ConflictBranch, result.ConflictBase,
				worktreeSuffix(result.ConflictDir))

			state.CurrentBranchIndex = result.ConflictIdx
			state.ConflictBranch = result.ConflictBranch
			state.ConflictWorktree = result.ConflictDir
			state.RemainingBranches = result.Remaining
			state.UseOnto = result.NeedsOnto
			state.OntoOldBase = result.OntoOldBase
			if err := saveRebaseState(gitDir, state); err != nil {
				cfg.Warningf("failed to save rebase state: %s", err)
			}

			printConflictDetails(cfg, result.ConflictBase, result.ConflictDir)
			cfg.Printf("")
			reportConflictLocation(cfg, result.ConflictBranch, result.ConflictDir)
			cfg.Printf("Or abort this operation with `%s`",
				cfg.ColorCyan("gh stack rebase --abort"))
			return ErrConflict
		}
	}

	_ = git.CheckoutBranch(state.OriginalBranch)

	verifyStart, verifyEnd := state.StartIndex, state.EndIndex
	if verifyEnd <= verifyStart {
		verifyStart, verifyEnd = 0, len(s.Branches)
		if state.NoTrunk {
			verifyStart = 1
		}
	}
	if unstacked := verifyStacked(s, trunkBase, verifyStart, verifyEnd); len(unstacked) > 0 {
		reportUnstacked(cfg, trunkRef, unstacked)
		restoreRebaseRefs(cfg, state.OriginalBranch, state.OriginalRefs, worktrees)
		clearRebaseState(gitDir)
		return ErrSilent
	}

	clearRebaseState(gitDir)
	updateBaseSHAs(s)

	_ = syncStackPRs(cfg, s)

	stack.SaveNonBlocking(gitDir, sf)

	if state.NoTrunk {
		cfg.Printf("All branches in stack rebased locally (without trunk)")
	} else if state.TrunkSHA != "" {
		cfg.Printf("All branches in stack rebased locally with %s (%s)", trunkRef, short(state.TrunkSHA))
	} else {
		cfg.Printf("All branches in stack rebased locally with %s", trunkRef)
	}
	cfg.Printf("To push up your changes and open/update the stack of PRs, run `%s`",
		cfg.ColorCyan("gh stack submit"))

	return nil
}

func abortRebase(cfg *config.Config, gitDir string) error {
	state, err := loadRebaseState(gitDir)
	if err != nil {
		cfg.Errorf("no rebase in progress")
		return ErrSilent
	}

	// Abort where the rebase actually stopped: its state lives in the worktree
	// that owns the conflicting branch.
	if git.IsRebaseInProgressIn(state.ConflictWorktree) {
		_ = git.RebaseAbortIn(state.ConflictWorktree)
	}

	// The abort above reattached the conflicting branch, so the worktree map is
	// built after it to see that branch again.
	currentBranch, _ := git.CurrentBranch()
	worktrees := newWorktreeIndex(currentBranch)

	var restoreErrors []string
	for branch, sha := range state.OriginalRefs {
		if dir := worktrees.DirFor(branch); dir != "" {
			if err := git.ResetHardIn(dir, sha); err != nil {
				restoreErrors = append(restoreErrors, fmt.Sprintf("reset %s in %s: %s", branch, dir, err))
			}
			continue
		}
		if err := git.CheckoutBranch(branch); err != nil {
			restoreErrors = append(restoreErrors, fmt.Sprintf("checkout %s: %s", branch, err))
			continue
		}
		if err := git.ResetHard(sha); err != nil {
			restoreErrors = append(restoreErrors, fmt.Sprintf("reset %s: %s", branch, err))
		}
	}

	_ = git.CheckoutBranch(state.OriginalBranch)
	clearRebaseState(gitDir)

	if len(restoreErrors) > 0 {
		cfg.Warningf("Rebase aborted but some branches could not be fully restored:")
		for _, e := range restoreErrors {
			cfg.Printf("  %s", e)
		}
		return ErrSilent
	}

	cfg.Successf("Rebase aborted and branches restored")
	return nil
}

func saveRebaseState(gitDir string, state *rebaseState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("error serializing rebase state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, rebaseStateFile), data, 0644); err != nil {
		return fmt.Errorf("error writing rebase state: %w", err)
	}
	return nil
}

func loadRebaseState(gitDir string) (*rebaseState, error) {
	data, err := os.ReadFile(filepath.Join(gitDir, rebaseStateFile))
	if err != nil {
		return nil, err
	}
	var state rebaseState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func clearRebaseState(gitDir string) {
	_ = os.Remove(filepath.Join(gitDir, rebaseStateFile))
}

// reportConflictLocation tells the user where to resolve the conflict. When the
// branch belongs to another worktree, the files to edit are in that directory,
// even though `gh stack rebase --continue` still works from anywhere.
func reportConflictLocation(cfg *config.Config, branch, dir string) {
	if dir == "" {
		cfg.Printf("Resolve conflicts on %s, then run `%s`",
			branch, cfg.ColorCyan("gh stack rebase --continue"))
		return
	}
	cfg.Printf("Resolve conflicts on %s in %s, then run `%s` (from any worktree)",
		branch, dir, cfg.ColorCyan("gh stack rebase --continue"))
}

func printConflictDetails(cfg *config.Config, branch, dir string) {
	printConflictDetailsWithContinue(cfg, branch, dir, "gh stack rebase --continue")
}

// printConflictDetailsWithContinue lists the conflicted files of the rebase
// running in dir ("" for the current worktree) and explains how to resolve them.
func printConflictDetailsWithContinue(cfg *config.Config, branch, dir string, continueCmd string) {
	files, err := git.ConflictedFilesIn(dir)
	if err == nil && len(files) > 0 {
		cfg.Printf("")
		cfg.Printf("%s", cfg.ColorBold("Conflicted files:"))
		for _, f := range files {
			// git reports paths relative to the worktree it ran in, so they are
			// resolved against that worktree to be readable — and printed that
			// way, since the user has to open them there.
			path := f
			if dir != "" {
				path = filepath.Join(dir, f)
			}
			info, err := git.FindConflictMarkers(path)
			if err != nil || len(info.Sections) == 0 {
				cfg.Printf("  %s %s", cfg.ColorWarning("C"), path)
				continue
			}
			for _, sec := range info.Sections {
				cfg.Printf("  %s %s (lines %d–%d)",
					cfg.ColorWarning("C"), path, sec.StartLine, sec.EndLine)
			}
		}
	}

	cfg.Printf("")
	cfg.Printf("%s", cfg.ColorBold("To resolve:"))
	cfg.Printf("  1. Open each conflicted file and look for conflict markers:")
	cfg.Printf("     %s  (incoming changes from %s)", cfg.ColorCyan("<<<<<<< HEAD"), branch)
	cfg.Printf("     %s", cfg.ColorCyan("======="))
	cfg.Printf("     %s  (changes being rebased)", cfg.ColorCyan(">>>>>>>"))
	cfg.Printf("  2. Edit the file to keep the desired changes and remove the markers")
	cfg.Printf("  3. Stage resolved files: `%s`", cfg.ColorCyan("git add <file>"))
	cfg.Printf("  4. Continue:  `%s`", cfg.ColorCyan(continueCmd))
}

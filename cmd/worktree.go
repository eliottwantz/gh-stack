package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/stack"
)

// stackDir returns the directory gh-stack keeps its state in: the git common
// directory, which every worktree of the repository shares. The per-worktree
// git directory (.git/worktrees/<name>) would hide a stack created in one
// worktree from all the others, so it is never used for stack state.
//
// A stack file left behind in the current worktree's private git directory by
// an older gh-stack is moved into the shared location on the way.
func stackDir(cfg *config.Config) (string, error) {
	common, err := git.CommonDir()
	if err != nil {
		return "", err
	}
	migrateWorktreeStackFile(cfg, common)
	return common, nil
}

// migrateWorktreeStackFile relocates a stack file written into this worktree's
// private git directory before gh-stack shared its state across worktrees.
// Best-effort: any failure leaves the old file in place and is reported as a
// warning, since the shared file is still usable.
func migrateWorktreeStackFile(cfg *config.Config, common string) {
	gitDir, err := git.GitDir()
	if err != nil {
		return
	}
	if abs, absErr := filepath.Abs(gitDir); absErr == nil {
		gitDir = abs
	}
	if gitDir == common {
		return
	}
	migrated, err := stack.MigrateFromWorktree(gitDir, common)
	if err != nil {
		cfg.Warningf("Could not move this worktree's stack file to %s: %v", common, err)
		return
	}
	if migrated {
		cfg.Infof("Moved this worktree's stack file to %s so every worktree of the repository shares it", common)
	}
}

// worktreeOwner is the worktree holding one branch.
type worktreeOwner struct {
	// Dir is the root directory of the worktree.
	Dir string
	// Rebasing reports that the worktree is in the middle of rebasing this
	// branch, so its HEAD is detached and the branch must not be touched.
	Rebasing bool
}

// worktreeIndex records which branches are checked out in worktrees other than
// the one a command is running in. git refuses to check out, rebase, delete or
// force-update a branch that another worktree holds, so those branches have to
// be operated on from inside the worktree that owns them.
//
// A nil *worktreeIndex is usable and reports every branch as free, which is the
// behavior in a repository with a single working tree.
type worktreeIndex struct {
	byBranch map[string]worktreeOwner
}

// newWorktreeIndex builds the index for the current repository. The worktree the
// command runs in is excluded — its branch is operated on in place, as before —
// and identified by its root directory, falling back to currentBranch when the
// root cannot be resolved.
//
// Discovery failures are not fatal: an empty index means "no other worktrees",
// which is the behavior gh-stack had before it understood worktrees.
func newWorktreeIndex(currentBranch string) *worktreeIndex {
	idx := &worktreeIndex{byBranch: make(map[string]worktreeOwner)}
	worktrees, err := git.Worktrees()
	if err != nil {
		return idx
	}
	root, rootErr := git.RootDir()
	for _, wt := range worktrees {
		// A bare worktree has no branch and a prunable one has lost its
		// directory, so neither can own a stack branch.
		if wt.Bare || wt.Prunable {
			continue
		}
		if (rootErr == nil && wt.Path == root) || (currentBranch != "" && wt.Branch == currentBranch) {
			continue
		}
		if wt.Branch != "" {
			idx.byBranch[wt.Branch] = worktreeOwner{Dir: wt.Path}
			continue
		}
		// A rebasing worktree reports a detached HEAD, hiding the branch it is
		// rewriting. Claim that branch too, so a cascade here does not rewrite
		// it underneath the rebase running there.
		if branch, err := git.RebasingBranchIn(wt.Path); err == nil && branch != "" {
			idx.byBranch[branch] = worktreeOwner{Dir: wt.Path, Rebasing: true}
		}
	}
	return idx
}

// DirFor returns the root directory of the worktree that owns branch, or "" when
// the branch is free for the current worktree to operate on directly. The
// result is meant to be passed straight to the git package's *In helpers, which
// treat "" as "operate here".
func (w *worktreeIndex) DirFor(branch string) string {
	if w == nil {
		return ""
	}
	return w.byBranch[branch].Dir
}

// Empty reports whether no other worktree holds a branch of this repository.
func (w *worktreeIndex) Empty() bool {
	return w == nil || len(w.byBranch) == 0
}

// checkWorktreesReady verifies that every branch owned by another worktree can
// be rebased there: no rebase in progress, and no uncommitted changes (git
// refuses to start a rebase in a dirty worktree). Checking up front matters
// because a cascade that stops halfway has already rewritten the branches below
// the failure.
func checkWorktreesReady(cfg *config.Config, w *worktreeIndex, branches []string) error {
	if w.Empty() {
		return nil
	}

	var blocked []string
	for _, branch := range branches {
		owner, ok := w.byBranch[branch]
		if !ok {
			continue
		}
		if owner.Rebasing || git.IsRebaseInProgressIn(owner.Dir) {
			blocked = append(blocked, fmt.Sprintf("%s — a rebase is already in progress in %s", branch, owner.Dir))
			continue
		}
		dirty, err := git.HasUncommittedChangesIn(owner.Dir)
		if err != nil {
			blocked = append(blocked, fmt.Sprintf("%s — could not inspect the worktree at %s: %v", branch, owner.Dir, err))
			continue
		}
		if dirty {
			blocked = append(blocked, fmt.Sprintf("%s — uncommitted changes in %s", branch, owner.Dir))
		}
	}

	if len(blocked) == 0 {
		return nil
	}

	cfg.Errorf("some branches are checked out in worktrees that cannot be rebased right now:")
	for _, b := range blocked {
		cfg.Printf("  %s", b)
	}
	cfg.Printf("  Commit, stash, or finish the work in those worktrees, then run this command again.")
	return ErrSilent
}

// checkNoBranchesInOtherWorktrees fails when any branch of the stack is checked
// out in another worktree. It is for operations that cannot be delegated to the
// owning worktree — renaming, dropping, or reordering branches — as opposed to
// a plain rebase, which can.
func checkNoBranchesInOtherWorktrees(cfg *config.Config, currentBranch string, s *stack.Stack) error {
	worktrees := newWorktreeIndex(currentBranch)
	if worktrees.Empty() {
		return nil
	}

	var held []string
	for _, br := range s.Branches {
		if dir := worktrees.DirFor(br.Branch); dir != "" {
			held = append(held, fmt.Sprintf("%s — %s", br.Branch, dir))
		}
	}
	if len(held) == 0 {
		return nil
	}

	cfg.Errorf("some branches of this stack are checked out in other worktrees:")
	for _, h := range held {
		cfg.Printf("  %s", h)
	}
	cfg.Printf("  Switch those worktrees to another branch, or remove them with `%s`, then try again.",
		cfg.ColorCyan("git worktree remove"))
	return ErrSilent
}

// reportCheckoutFailure explains a failed checkout. When another worktree has
// the branch, git only says the checkout is not possible, so name the directory
// the branch actually lives in — with one worktree per pull request that is the
// most common reason a checkout fails.
func reportCheckoutFailure(cfg *config.Config, branch string, err error) {
	// Only consulted after a failure, to keep the common path free of the
	// worktree lookup.
	if dir := newWorktreeIndex("").DirFor(branch); dir != "" {
		cfg.Errorf("%s is checked out in another worktree", branch)
		cfg.Printf("  Work on it there: `%s`", cfg.ColorCyan("cd "+dir))
		return
	}
	cfg.Errorf("failed to checkout %s: %v", branch, err)
}

// worktreeSuffix returns a short " (in <path>)" note for user-facing messages
// about a branch another worktree owns, or "" for a branch of this worktree.
func worktreeSuffix(dir string) string {
	if dir == "" {
		return ""
	}
	return fmt.Sprintf(" (in %s)", dir)
}

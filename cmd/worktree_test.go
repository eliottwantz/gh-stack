package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/stack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWorktreeMock returns a mock whose repository is rooted at rootDir and
// reports the given worktrees.
func newWorktreeMock(rootDir string, worktrees ...git.Worktree) *git.MockOps {
	return &git.MockOps{
		RootDirFn:   func() (string, error) { return rootDir, nil },
		WorktreesFn: func() ([]git.Worktree, error) { return worktrees, nil },
	}
}

func TestNewWorktreeIndex(t *testing.T) {
	t.Run("maps branches held by other worktrees", func(t *testing.T) {
		mock := newWorktreeMock("/repo",
			git.Worktree{Path: "/repo", Branch: "b1"},
			git.Worktree{Path: "/wt/b2", Branch: "b2"},
		)
		defer git.SetOps(mock)()

		idx := newWorktreeIndex("b1")

		assert.Equal(t, "/wt/b2", idx.DirFor("b2"))
		assert.Empty(t, idx.DirFor("b1"), "the current worktree's branch stays local")
		assert.Empty(t, idx.DirFor("b3"), "an unheld branch is free")
		assert.False(t, idx.Empty())
	})

	t.Run("excludes the current worktree by path even on a detached HEAD", func(t *testing.T) {
		mock := newWorktreeMock("/repo",
			git.Worktree{Path: "/repo", Detached: true},
			git.Worktree{Path: "/wt/b2", Branch: "b2"},
		)
		// The current worktree is mid-rebase of b1; it must not be treated as
		// another worktree holding b1.
		mock.RebasingBranchInDirFn = func(dir string) (string, error) {
			if dir == "/repo" {
				return "b1", nil
			}
			return "", nil
		}
		defer git.SetOps(mock)()

		idx := newWorktreeIndex("")

		assert.Empty(t, idx.DirFor("b1"))
		assert.Equal(t, "/wt/b2", idx.DirFor("b2"))
	})

	t.Run("claims the branch a detached worktree is rebasing", func(t *testing.T) {
		mock := newWorktreeMock("/repo",
			git.Worktree{Path: "/repo", Branch: "b1"},
			git.Worktree{Path: "/wt/b2", Detached: true},
		)
		mock.RebasingBranchInDirFn = func(dir string) (string, error) {
			if dir == "/wt/b2" {
				return "b2", nil
			}
			return "", nil
		}
		defer git.SetOps(mock)()

		idx := newWorktreeIndex("b1")

		assert.Equal(t, "/wt/b2", idx.DirFor("b2"),
			"a worktree rebasing a branch still owns it, even though git reports it detached")
	})

	t.Run("ignores bare and prunable worktrees", func(t *testing.T) {
		mock := newWorktreeMock("/repo",
			git.Worktree{Path: "/repo", Branch: "b1"},
			git.Worktree{Path: "/bare", Bare: true},
			git.Worktree{Path: "/gone", Branch: "b2", Prunable: true},
		)
		defer git.SetOps(mock)()

		idx := newWorktreeIndex("b1")

		assert.True(t, idx.Empty())
		assert.Empty(t, idx.DirFor("b2"))
	})

	t.Run("discovery failure degrades to no worktrees", func(t *testing.T) {
		mock := &git.MockOps{
			WorktreesFn: func() ([]git.Worktree, error) { return nil, assert.AnError },
		}
		defer git.SetOps(mock)()

		assert.True(t, newWorktreeIndex("b1").Empty())
	})

	t.Run("nil index reports every branch as free", func(t *testing.T) {
		var idx *worktreeIndex
		assert.True(t, idx.Empty())
		assert.Empty(t, idx.DirFor("b1"))
	})
}

func TestCheckWorktreesReady(t *testing.T) {
	worktrees := []git.Worktree{
		{Path: "/repo", Branch: "b1"},
		{Path: "/wt/b2", Branch: "b2"},
	}

	t.Run("passes when the other worktree is clean", func(t *testing.T) {
		mock := newWorktreeMock("/repo", worktrees...)
		defer git.SetOps(mock)()

		cfg, _, _ := config.NewTestConfig()
		assert.NoError(t, checkWorktreesReady(cfg, newWorktreeIndex("b1"), []string{"b1", "b2"}))
	})

	t.Run("refuses to start when a worktree has uncommitted changes", func(t *testing.T) {
		mock := newWorktreeMock("/repo", worktrees...)
		mock.HasUncommittedChangesInDirFn = func(dir string) (bool, error) {
			return dir == "/wt/b2", nil
		}
		defer git.SetOps(mock)()

		cfg, outR, errR := config.NewTestConfig()
		err := checkWorktreesReady(cfg, newWorktreeIndex("b1"), []string{"b1", "b2"})

		assert.ErrorIs(t, err, ErrSilent)
		output := readCfgOutput(cfg, outR, errR)
		assert.Contains(t, output, "b2")
		assert.Contains(t, output, "/wt/b2")
		assert.Contains(t, output, "uncommitted changes")
	})

	t.Run("refuses to start when a worktree is mid-rebase", func(t *testing.T) {
		mock := newWorktreeMock("/repo", worktrees...)
		mock.IsRebaseInProgressInDirFn = func(dir string) bool { return dir == "/wt/b2" }
		defer git.SetOps(mock)()

		cfg, outR, errR := config.NewTestConfig()
		err := checkWorktreesReady(cfg, newWorktreeIndex("b1"), []string{"b1", "b2"})

		assert.ErrorIs(t, err, ErrSilent)
		assert.Contains(t, readCfgOutput(cfg, outR, errR), "rebase is already in progress")
	})

	t.Run("ignores worktrees holding branches outside the range", func(t *testing.T) {
		mock := newWorktreeMock("/repo", worktrees...)
		mock.HasUncommittedChangesInDirFn = func(dir string) (bool, error) { return true, nil }
		defer git.SetOps(mock)()

		cfg, _, _ := config.NewTestConfig()
		assert.NoError(t, checkWorktreesReady(cfg, newWorktreeIndex("b1"), []string{"b1"}))
	})
}

// stackDir must resolve to the common git directory so a stack created in one
// worktree is visible from all of them.
func TestStackDir_UsesCommonDir(t *testing.T) {
	mock := &git.MockOps{
		GitDirFn:    func() (string, error) { return "/repo/.git/worktrees/b2", nil },
		CommonDirFn: func() (string, error) { return "/repo/.git", nil },
	}
	defer git.SetOps(mock)()

	cfg, _, _ := config.NewTestConfig()
	dir, err := stackDir(cfg)

	require.NoError(t, err)
	assert.Equal(t, "/repo/.git", dir)
}

// A stack file written by an older gh-stack lives in the worktree's private git
// directory, where the other worktrees cannot see it.
func TestStackDir_MigratesStackFileFromWorktree(t *testing.T) {
	commonDir := t.TempDir()
	worktreeGitDir := t.TempDir()
	writeStackFile(t, worktreeGitDir, stack.Stack{
		Trunk:    stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{{Branch: "b1"}},
	})

	mock := &git.MockOps{
		GitDirFn:    func() (string, error) { return worktreeGitDir, nil },
		CommonDirFn: func() (string, error) { return commonDir, nil },
	}
	defer git.SetOps(mock)()

	cfg, outR, errR := config.NewTestConfig()
	dir, err := stackDir(cfg)
	require.NoError(t, err)
	assert.Equal(t, commonDir, dir)

	sf, err := stack.Load(commonDir)
	require.NoError(t, err)
	require.Len(t, sf.Stacks, 1)
	assert.Equal(t, "b1", sf.Stacks[0].Branches[0].Branch)

	_, statErr := os.Stat(filepath.Join(worktreeGitDir, "gh-stack"))
	assert.True(t, os.IsNotExist(statErr), "the worktree copy should be gone")
	assert.Contains(t, readCfgOutput(cfg, outR, errR), "Moved this worktree's stack file")
}

// An existing shared stack file is never overwritten by a worktree-local one.
func TestStackDir_KeepsExistingCommonStackFile(t *testing.T) {
	commonDir := t.TempDir()
	worktreeGitDir := t.TempDir()
	writeStackFile(t, commonDir, stack.Stack{
		Trunk:    stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{{Branch: "shared"}},
	})
	writeStackFile(t, worktreeGitDir, stack.Stack{
		Trunk:    stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{{Branch: "local"}},
	})

	mock := &git.MockOps{
		GitDirFn:    func() (string, error) { return worktreeGitDir, nil },
		CommonDirFn: func() (string, error) { return commonDir, nil },
	}
	defer git.SetOps(mock)()

	cfg, _, _ := config.NewTestConfig()
	_, err := stackDir(cfg)
	require.NoError(t, err)

	sf, err := stack.Load(commonDir)
	require.NoError(t, err)
	require.Len(t, sf.Stacks, 1)
	assert.Equal(t, "shared", sf.Stacks[0].Branches[0].Branch)
	assert.FileExists(t, filepath.Join(worktreeGitDir, "gh-stack"))
}

// ---------------------------------------------------------------------------
// Command-level behavior
// ---------------------------------------------------------------------------

// rebaseInDirCall records a rebase routed into a specific worktree.
type rebaseInDirCall struct {
	dir    string
	branch string
	base   string
}

// TestRebase_RebasesBranchInsideItsWorktree is the core worktree scenario: one
// worktree per pull request, so the branches below the top of the stack are
// checked out elsewhere and cannot be rewritten from here.
func TestRebase_RebasesBranchInsideItsWorktree(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1"},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var scoped []rebaseInDirCall
	var unscoped []rebaseCall
	var checkouts []string

	mock := newRebaseMock(tmpDir, "b1")
	mock.RootDirFn = func() (string, error) { return "/repo", nil }
	mock.WorktreesFn = func() ([]git.Worktree, error) {
		return []git.Worktree{
			{Path: "/repo", Branch: "b1"},
			{Path: "/wt/b2", Branch: "b2"},
		}, nil
	}
	mock.CheckoutBranchFn = func(name string) error {
		checkouts = append(checkouts, name)
		return nil
	}
	mock.RebaseFn = func(base string, opts git.RebaseOpts) error {
		unscoped = append(unscoped, rebaseCall{newBase: base, branch: "b1"})
		return nil
	}
	mock.RebaseOntoFn = func(newBase, oldBase, branch string, opts git.RebaseOpts) error {
		unscoped = append(unscoped, rebaseCall{newBase, oldBase, branch})
		return nil
	}
	mock.RebaseOntoInDirFn = func(dir, newBase, oldBase, branch string, opts git.RebaseOpts) error {
		scoped = append(scoped, rebaseInDirCall{dir: dir, branch: branch, base: newBase})
		return nil
	}

	defer git.SetOps(mock)()

	cfg, outR, errR := config.NewTestConfig()
	cmd := RebaseCmd(cfg)
	cmd.SetArgs([]string{"--no-trunk"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	require.NoError(t, cmd.Execute())

	require.Len(t, scoped, 1, "only b2 lives in another worktree")
	assert.Equal(t, rebaseInDirCall{dir: "/wt/b2", branch: "b2", base: "b1"}, scoped[0])

	// b3 is free, so it is rebased here as before.
	require.Len(t, unscoped, 1)
	assert.Equal(t, "b3", unscoped[0].branch)
	assert.Equal(t, "b2", unscoped[0].newBase)

	assert.NotContains(t, checkouts, "b2", "checking out another worktree's branch would fail")
	assert.Contains(t, readCfgOutput(cfg, outR, errR), "/wt/b2")
}

// A dirty worktree cannot be rebased, and finding that out halfway through a
// cascade would leave the stack partly rewritten.
func TestRebase_StopsBeforeRewritingWhenWorktreeIsDirty(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1"},
			{Branch: "b2"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var rebases int

	mock := newRebaseMock(tmpDir, "b1")
	mock.RootDirFn = func() (string, error) { return "/repo", nil }
	mock.WorktreesFn = func() ([]git.Worktree, error) {
		return []git.Worktree{
			{Path: "/repo", Branch: "b1"},
			{Path: "/wt/b2", Branch: "b2"},
		}, nil
	}
	mock.HasUncommittedChangesInDirFn = func(dir string) (bool, error) { return dir == "/wt/b2", nil }
	mock.CheckoutBranchFn = func(string) error { return nil }
	mock.RebaseFn = func(string, git.RebaseOpts) error { rebases++; return nil }
	mock.RebaseOntoFn = func(string, string, string, git.RebaseOpts) error { rebases++; return nil }
	mock.RebaseOntoInDirFn = func(string, string, string, string, git.RebaseOpts) error { rebases++; return nil }

	defer git.SetOps(mock)()

	cfg, outR, errR := config.NewTestConfig()
	cmd := RebaseCmd(cfg)
	cmd.SetArgs([]string{"--no-trunk"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	assert.ErrorIs(t, err, ErrSilent)
	assert.Zero(t, rebases, "nothing should be rewritten when a worktree is not ready")
	assert.Contains(t, readCfgOutput(cfg, outR, errR), "uncommitted changes in /wt/b2")
}

// A conflict raised in another worktree must be recorded, because that is the
// only worktree where `git rebase --continue` can finish it.
func TestRebase_ConflictInWorktree_SavesAndContinuesThere(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1"},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	worktrees := func() ([]git.Worktree, error) {
		return []git.Worktree{
			{Path: "/repo", Branch: "b1"},
			{Path: "/wt/b2", Branch: "b2"},
		}, nil
	}

	mock := newRebaseMock(tmpDir, "b1")
	mock.RootDirFn = func() (string, error) { return "/repo", nil }
	mock.WorktreesFn = worktrees
	mock.CheckoutBranchFn = func(string) error { return nil }
	mock.RebaseFn = func(string, git.RebaseOpts) error { return nil }
	mock.RebaseOntoInDirFn = func(dir, newBase, oldBase, branch string, opts git.RebaseOpts) error {
		return assert.AnError // b2 conflicts in its worktree
	}
	mock.ConflictedFilesInDirFn = func(dir string) ([]string, error) {
		assert.Equal(t, "/wt/b2", dir, "conflicted files live in the rebasing worktree")
		return nil, nil
	}
	defer git.SetOps(mock)()

	cfg, outR, errR := config.NewTestConfig()
	cmd := RebaseCmd(cfg)
	cmd.SetArgs([]string{"--no-trunk"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	assert.ErrorIs(t, err, ErrConflict)
	output := readCfgOutput(cfg, outR, errR)
	assert.Contains(t, output, "Resolve conflicts on b2 in /wt/b2")

	stateData, readErr := os.ReadFile(filepath.Join(tmpDir, rebaseStateFile))
	require.NoError(t, readErr)
	var state rebaseState
	require.NoError(t, json.Unmarshal(stateData, &state))
	assert.Equal(t, "b2", state.ConflictBranch)
	assert.Equal(t, "/wt/b2", state.ConflictWorktree)

	// --- Now continue from the main worktree, where no rebase is in progress ---
	var continuedIn string
	contMock := newRebaseMock(tmpDir, "b1")
	contMock.RootDirFn = func() (string, error) { return "/repo", nil }
	contMock.WorktreesFn = worktrees
	contMock.CheckoutBranchFn = func(string) error { return nil }
	contMock.IsRebaseInProgressInDirFn = func(dir string) bool { return dir == "/wt/b2" }
	contMock.RebaseContinueInDirFn = func(dir string, opts git.RebaseOpts) error {
		continuedIn = dir
		return nil
	}
	contMock.RebaseOntoFn = func(string, string, string, git.RebaseOpts) error { return nil }
	defer git.SetOps(contMock)()

	contCfg, contOutR, contErrR := config.NewTestConfig()
	contCmd := RebaseCmd(contCfg)
	contCmd.SetArgs([]string{"--continue"})
	contCmd.SetOut(io.Discard)
	contCmd.SetErr(io.Discard)
	require.NoError(t, contCmd.Execute())

	assert.Equal(t, "/wt/b2", continuedIn, "the interrupted rebase only exists in that worktree")
	assert.Contains(t, readCfgOutput(contCfg, contOutR, contErrR), "/wt/b2")
	assert.NoFileExists(t, filepath.Join(tmpDir, rebaseStateFile))
}

// --abort must reach into the worktree that holds the interrupted rebase and
// restore branches there instead of checking them out locally.
func TestRebase_AbortRestoresBranchesInTheirWorktrees(t *testing.T) {
	tmpDir := t.TempDir()

	state := &rebaseState{
		CurrentBranchIndex: 1,
		ConflictBranch:     "b2",
		ConflictWorktree:   "/wt/b2",
		RemainingBranches:  []string{"b3"},
		OriginalBranch:     "b1",
		OriginalRefs: map[string]string{
			"b1": "orig-sha-b1",
			"b2": "orig-sha-b2",
		},
	}
	stateData, err := json.MarshalIndent(state, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, rebaseStateFile), stateData, 0644))

	var abortedIn string
	var localResets []resetCall
	scopedResets := map[string]string{}
	currentBranch := "b1"

	mock := newRebaseMock(tmpDir, currentBranch)
	mock.RootDirFn = func() (string, error) { return "/repo", nil }
	mock.WorktreesFn = func() ([]git.Worktree, error) {
		return []git.Worktree{
			{Path: "/repo", Branch: "b1"},
			{Path: "/wt/b2", Branch: "b2"},
		}, nil
	}
	mock.IsRebaseInProgressInDirFn = func(dir string) bool { return dir == "/wt/b2" }
	mock.RebaseAbortInDirFn = func(dir string) error {
		abortedIn = dir
		return nil
	}
	mock.CheckoutBranchFn = func(name string) error {
		currentBranch = name
		return nil
	}
	mock.ResetHardFn = func(ref string) error {
		localResets = append(localResets, resetCall{currentBranch, ref})
		return nil
	}
	mock.ResetHardInDirFn = func(dir, ref string) error {
		scopedResets[dir] = ref
		return nil
	}
	defer git.SetOps(mock)()

	cfg, outR, errR := config.NewTestConfig()
	cmd := RebaseCmd(cfg)
	cmd.SetArgs([]string{"--abort"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	require.NoError(t, cmd.Execute())

	assert.Equal(t, "/wt/b2", abortedIn)
	assert.Equal(t, map[string]string{"/wt/b2": "orig-sha-b2"}, scopedResets)
	require.Len(t, localResets, 1)
	assert.Equal(t, resetCall{"b1", "orig-sha-b1"}, localResets[0])
	assert.Contains(t, readCfgOutput(cfg, outR, errR), "Rebase aborted")
	assert.NoFileExists(t, filepath.Join(tmpDir, rebaseStateFile))
}

// Sync deletes merged branches with --prune, which git refuses for a branch
// another worktree has checked out.
func TestSync_DoesNotPruneBranchHeldByAnotherWorktree(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1", PullRequest: &stack.PullRequestRef{Number: 1, Merged: true}},
			{Branch: "b2"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var deleted []string

	mock := newSyncMock(tmpDir, "b2")
	mock.RootDirFn = func() (string, error) { return "/repo", nil }
	mock.WorktreesFn = func() ([]git.Worktree, error) {
		return []git.Worktree{
			{Path: "/repo", Branch: "b2"},
			{Path: "/wt/b1", Branch: "b1"},
		}, nil
	}
	mock.CheckoutBranchFn = func(string) error { return nil }
	mock.RebaseFn = func(string, git.RebaseOpts) error { return nil }
	mock.RebaseOntoFn = func(string, string, string, git.RebaseOpts) error { return nil }
	mock.RebaseOntoInDirFn = func(string, string, string, string, git.RebaseOpts) error { return nil }
	mock.DeleteBranchFn = func(name string, force bool) error {
		deleted = append(deleted, name)
		return nil
	}
	defer git.SetOps(mock)()

	cfg, outR, errR := config.NewTestConfig()
	cmd := SyncCmd(cfg)
	cmd.SetArgs([]string{"--prune"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	require.NoError(t, cmd.Execute())

	assert.Empty(t, deleted, "a branch checked out in another worktree must survive --prune")
	output := readCfgOutput(cfg, outR, errR)
	assert.Contains(t, output, "Not pruning b1")
	assert.Contains(t, output, "/wt/b1")
}

func TestReportCheckoutFailure_NamesTheOwningWorktree(t *testing.T) {
	mock := newWorktreeMock("/repo",
		git.Worktree{Path: "/repo", Branch: "b1"},
		git.Worktree{Path: "/wt/b2", Branch: "b2"},
	)
	defer git.SetOps(mock)()

	cfg, outR, errR := config.NewTestConfig()
	reportCheckoutFailure(cfg, "b2", assert.AnError)

	output := readCfgOutput(cfg, outR, errR)
	assert.Contains(t, output, "b2 is checked out in another worktree")
	assert.Contains(t, output, "cd /wt/b2")
}

func TestReportCheckoutFailure_FallsBackToTheGitError(t *testing.T) {
	mock := newWorktreeMock("/repo", git.Worktree{Path: "/repo", Branch: "b1"})
	defer git.SetOps(mock)()

	cfg, outR, errR := config.NewTestConfig()
	reportCheckoutFailure(cfg, "b2", assert.AnError)

	assert.Contains(t, readCfgOutput(cfg, outR, errR), "failed to checkout b2")
}

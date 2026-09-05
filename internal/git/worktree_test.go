package git

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWorktreeList(t *testing.T) {
	out := `worktree /repo
HEAD 1111111111111111111111111111111111111111
branch refs/heads/main

worktree /repo/../wt-feature
HEAD 2222222222222222222222222222222222222222
branch refs/heads/feature

worktree /wt-rebasing
HEAD 3333333333333333333333333333333333333333
detached

worktree /wt-gone
HEAD 4444444444444444444444444444444444444444
branch refs/heads/gone
prunable gitdir file points to non-existent location

worktree /bare
bare
`

	got := parseWorktreeList(out)
	require.Len(t, got, 5)

	assert.Equal(t, "/repo", got[0].Path)
	assert.Equal(t, "main", got[0].Branch)
	assert.Equal(t, "1111111111111111111111111111111111111111", got[0].Head)
	assert.False(t, got[0].Detached)

	// Paths are cleaned so they can be compared with the repository root.
	assert.Equal(t, filepath.Clean("/wt-feature"), got[1].Path)
	assert.Equal(t, "feature", got[1].Branch)

	assert.True(t, got[2].Detached)
	assert.Empty(t, got[2].Branch, "a detached worktree holds no branch")

	assert.True(t, got[3].Prunable, "prunable with a reason is still prunable")
	assert.Equal(t, "gone", got[3].Branch)

	assert.True(t, got[4].Bare)
	assert.Empty(t, got[4].Head)
}

func TestParseWorktreeList_EmptyOutput(t *testing.T) {
	assert.Empty(t, parseWorktreeList(""))
}

// setupWorktree creates a linked worktree holding a "feature" branch that is one
// commit ahead of main, then advances main. Returns the clone (main worktree)
// and the linked worktree directory. The process's working directory is the
// clone for the duration of the test.
func setupWorktree(t *testing.T) (cloneDir, wtDir string) {
	t.Helper()
	_, cloneDir = setupBareAndClone(t)
	t.Cleanup(withGitDir(t, cloneDir))

	gitExec(t, cloneDir, "config", "user.name", "Test")
	gitExec(t, cloneDir, "config", "user.email", "test@test.com")

	gitExec(t, cloneDir, "checkout", "-b", "feature")
	writeFile(t, cloneDir, "feature.txt", "feature\n")
	gitExec(t, cloneDir, "add", ".")
	gitExec(t, cloneDir, "commit", "-m", "feature work")
	gitExec(t, cloneDir, "checkout", "main")

	wtDir = filepath.Join(t.TempDir(), "wt-feature")
	gitExec(t, cloneDir, "worktree", "add", wtDir, "feature")

	writeFile(t, cloneDir, "main.txt", "main\n")
	gitExec(t, cloneDir, "add", ".")
	gitExec(t, cloneDir, "commit", "-m", "main work")

	return cloneDir, wtDir
}

func TestIntegration_Worktrees_ListsLinkedWorktree(t *testing.T) {
	cloneDir, wtDir := setupWorktree(t)

	list, err := Worktrees()
	require.NoError(t, err)
	require.Len(t, list, 2)

	byPath := map[string]Worktree{}
	for _, wt := range list {
		byPath[wt.Path] = wt
	}
	main, ok := byPath[cloneDir]
	require.True(t, ok, "main worktree %s missing from %v", cloneDir, list)
	assert.Equal(t, "main", main.Branch)

	linked, ok := byPath[wtDir]
	require.True(t, ok, "linked worktree %s missing from %v", wtDir, list)
	assert.Equal(t, "feature", linked.Branch)
	assert.False(t, linked.Bare)
	assert.False(t, linked.Prunable)
}

// gh-stack state must live in one place for the whole repository, so the common
// directory has to be the same from every worktree even though each worktree
// has its own private git directory.
func TestIntegration_CommonDirIsSharedAcrossWorktrees(t *testing.T) {
	cloneDir, wtDir := setupWorktree(t)

	fromMain, err := CommonDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cloneDir, ".git"), fromMain)

	restore := withGitDir(t, wtDir)
	defer restore()

	fromLinked, err := CommonDir()
	require.NoError(t, err)
	assert.Equal(t, fromMain, fromLinked, "every worktree must resolve the same common dir")

	privateDir, err := GitDir()
	require.NoError(t, err)
	privateDir, err = filepath.Abs(privateDir)
	require.NoError(t, err)
	assert.NotEqual(t, fromLinked, privateDir, "a linked worktree has its own private git dir")
}

// A branch checked out in another worktree cannot be rewritten from here, which
// is why the cascade rebases it inside the worktree that owns it.
func TestIntegration_RebaseOntoIn_RewritesBranchOwnedByAnotherWorktree(t *testing.T) {
	cloneDir, wtDir := setupWorktree(t)

	oldBase := gitExec(t, cloneDir, "rev-parse", "main~1")
	before := gitExec(t, cloneDir, "rev-parse", "feature")

	err := RebaseOnto("main", oldBase, "feature", RebaseOpts{})
	require.Error(t, err, "git must refuse to rebase a branch another worktree holds")

	require.NoError(t, RebaseOntoIn(wtDir, "main", oldBase, "feature", RebaseOpts{}))

	after := gitExec(t, cloneDir, "rev-parse", "feature")
	assert.NotEqual(t, before, after, "feature should have been rewritten")
	isAnc, err := IsAncestor("main", "feature")
	require.NoError(t, err)
	assert.True(t, isAnc, "feature should now contain main")

	// The worktree that owns the branch follows along instead of being left
	// pointing at the old commit.
	assert.Equal(t, after, gitExec(t, wtDir, "rev-parse", "HEAD"))
	assert.Equal(t, "feature", gitExec(t, wtDir, "rev-parse", "--abbrev-ref", "HEAD"))
}

func TestIntegration_RebaseIn_ConflictIsScopedToItsWorktree(t *testing.T) {
	cloneDir, wtDir := setupWorktree(t)

	// Both branches touch the same file, so rebasing conflicts.
	gitExec(t, wtDir, "config", "user.name", "Test")
	gitExec(t, wtDir, "config", "user.email", "test@test.com")
	writeFile(t, wtDir, "shared.txt", "from feature\n")
	gitExec(t, wtDir, "add", ".")
	gitExec(t, wtDir, "commit", "-m", "feature shared")
	writeFile(t, cloneDir, "shared.txt", "from main\n")
	gitExec(t, cloneDir, "add", ".")
	gitExec(t, cloneDir, "commit", "-m", "main shared")

	err := RebaseIn(wtDir, "main", RebaseOpts{})
	require.Error(t, err)
	assert.False(t, IsRebaseStartError(err), "the rebase started, then hit a conflict")

	assert.True(t, IsRebaseInProgressIn(wtDir))
	assert.False(t, IsRebaseInProgress(), "the current worktree is untouched")

	// git detaches HEAD while rebasing, so the branch is only discoverable
	// through the rebase state.
	list, err := Worktrees()
	require.NoError(t, err)
	for _, wt := range list {
		if wt.Path == wtDir {
			assert.True(t, wt.Detached)
			assert.Empty(t, wt.Branch)
		}
	}
	rebasing, err := RebasingBranchIn(wtDir)
	require.NoError(t, err)
	assert.Equal(t, "feature", rebasing)

	files, err := ConflictedFilesIn(wtDir)
	require.NoError(t, err)
	assert.Equal(t, []string{"shared.txt"}, files)

	require.NoError(t, RebaseAbortIn(wtDir))
	assert.False(t, IsRebaseInProgressIn(wtDir))

	rebasing, err = RebasingBranchIn(wtDir)
	require.NoError(t, err)
	assert.Empty(t, rebasing)
	assert.Equal(t, "feature", gitExec(t, wtDir, "rev-parse", "--abbrev-ref", "HEAD"))
}

func TestIntegration_ResetHardIn_RestoresBranchInItsWorktree(t *testing.T) {
	cloneDir, wtDir := setupWorktree(t)

	original := gitExec(t, cloneDir, "rev-parse", "feature")
	oldBase := gitExec(t, cloneDir, "rev-parse", "main~1")
	require.NoError(t, RebaseOntoIn(wtDir, "main", oldBase, "feature", RebaseOpts{}))
	require.NotEqual(t, original, gitExec(t, cloneDir, "rev-parse", "feature"))

	require.NoError(t, ResetHardIn(wtDir, original))
	assert.Equal(t, original, gitExec(t, cloneDir, "rev-parse", "feature"))
	assert.Equal(t, original, gitExec(t, wtDir, "rev-parse", "HEAD"))
}

func TestIntegration_HasUncommittedChangesIn_IsPerWorktree(t *testing.T) {
	_, wtDir := setupWorktree(t)

	dirty, err := HasUncommittedChangesIn(wtDir)
	require.NoError(t, err)
	assert.False(t, dirty)

	writeFile(t, wtDir, "feature.txt", "uncommitted edit\n")

	dirty, err = HasUncommittedChangesIn(wtDir)
	require.NoError(t, err)
	assert.True(t, dirty)

	dirty, err = HasUncommittedChanges()
	require.NoError(t, err)
	assert.False(t, dirty, "the current worktree is still clean")
}

func TestIntegration_MergeFFIn_FastForwardsBranchInItsWorktree(t *testing.T) {
	cloneDir, wtDir := setupWorktree(t)

	// Move feature forward from the worktree, then rewind the branch so the
	// main worktree sees it as behind a ref it can fast-forward to.
	behind := gitExec(t, cloneDir, "rev-parse", "feature")
	gitExec(t, wtDir, "config", "user.name", "Test")
	gitExec(t, wtDir, "config", "user.email", "test@test.com")
	writeFile(t, wtDir, "feature.txt", "more feature\n")
	gitExec(t, wtDir, "add", ".")
	gitExec(t, wtDir, "commit", "-m", "more feature work")
	ahead := gitExec(t, cloneDir, "rev-parse", "feature")
	gitExec(t, wtDir, "branch", "-f", "target", ahead)
	require.NoError(t, ResetHardIn(wtDir, behind))

	require.NoError(t, MergeFFIn(wtDir, "target"))
	assert.Equal(t, ahead, gitExec(t, cloneDir, "rev-parse", "feature"))
	assert.Equal(t, "feature", gitExec(t, wtDir, "rev-parse", "--abbrev-ref", "HEAD"))
}

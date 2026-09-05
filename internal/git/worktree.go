package git

import (
	"os"
	"path/filepath"
	"strings"
)

// Worktree describes one working tree attached to the repository, as reported
// by `git worktree list --porcelain`. The main working tree is the first entry
// returned by git; linked worktrees follow in creation order.
type Worktree struct {
	// Path is the absolute path to the worktree's root directory.
	Path string
	// Branch is the checked-out branch without the refs/heads/ prefix. It is
	// empty when the worktree has a detached HEAD or is bare.
	Branch string
	// Head is the HEAD commit SHA (empty for a bare main worktree).
	Head string
	// Bare reports whether this is a bare main worktree (no working files).
	Bare bool
	// Detached reports whether HEAD is detached in this worktree.
	Detached bool
	// Prunable reports whether git considers the worktree's administrative
	// files stale — its directory has usually been deleted from disk.
	Prunable bool
}

func (d *defaultOps) Worktrees() ([]Worktree, error) {
	out, err := run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeList(out), nil
}

// parseWorktreeList parses the porcelain output of `git worktree list`.
// Records are separated by blank lines and always start with a "worktree" line;
// attribute lines that this package does not use (such as "locked") are ignored.
func parseWorktreeList(out string) []Worktree {
	var (
		list []Worktree
		cur  *Worktree
	)
	flush := func() {
		if cur != nil {
			list = append(list, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &Worktree{Path: filepath.Clean(strings.TrimPrefix(line, "worktree "))}
		case cur == nil:
			// Attribute line without a preceding "worktree" line — ignore.
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "bare":
			cur.Bare = true
		case line == "detached":
			cur.Detached = true
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			cur.Prunable = true
		}
	}
	flush()
	return list
}

// RebasingBranchInDir returns the branch the worktree rooted at dir is in the
// middle of rebasing, or "" when no rebase is in progress there. git detaches a
// worktree's HEAD for the duration of a rebase, so `git worktree list` stops
// reporting the branch — reading the rebase state is the only way to tell that
// a branch is already being rewritten somewhere else.
func (d *defaultOps) RebasingBranchInDir(dir string) (string, error) {
	gitDir, err := gitDirIn(dir)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		data, err := os.ReadFile(filepath.Join(gitDir, name, "head-name"))
		if err != nil {
			continue
		}
		ref := strings.TrimSpace(string(data))
		// A rebase started from a detached HEAD records "detached HEAD"
		// instead of a ref, and holds no branch.
		if ref == "" || !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		return strings.TrimPrefix(ref, "refs/heads/"), nil
	}
	return "", nil
}

// CommonDir returns the repository's common git directory: the .git directory
// of the main working tree, shared by every linked worktree. In a repository
// without worktrees it is the same as GitDir().
func (d *defaultOps) CommonDir() (string, error) {
	out, err := run("rev-parse", "--git-common-dir")
	if err != nil {
		// Older git, or a failure that GitDir can still answer.
		return d.GitDir()
	}
	return absDir("", out)
}

// gitDirIn returns the absolute git directory of the worktree rooted at dir.
// An empty dir resolves the git directory of the current working directory.
func gitDirIn(dir string) (string, error) {
	if dir == "" {
		return GitDir()
	}
	out, err := runIn(dir, "rev-parse", "--git-dir")
	if err != nil {
		return "", err
	}
	return absDir(dir, out)
}

// absDir makes a path git printed absolute. git resolves such paths relative to
// the directory it ran in, which is base ("" meaning the process's own working
// directory).
func absDir(base, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if base != "" {
		return filepath.Join(base, path), nil
	}
	return filepath.Abs(path)
}

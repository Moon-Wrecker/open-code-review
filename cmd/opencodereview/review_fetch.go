// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/alibaba/open-code-review/internal/agent"
	"github.com/alibaba/open-code-review/internal/diff"
	"github.com/alibaba/open-code-review/internal/stdout"
)

// fetchTarget is the remote branch that review --fetch refreshes.
type fetchTarget struct {
	remote string
	branch string
}

// trackingRef is where the fetched branch lands, spelled in full because the
// short form can be shadowed by a local branch of the same name.
func (t fetchTarget) trackingRef() string {
	return "refs/remotes/" + t.remote + "/" + t.branch
}

// parseFetchTarget reads --from as a branch on a configured remote. An explicit
// --remote is authoritative: --from is then a branch on it, minus an optional
// "<remote>/" prefix naming that same remote, so every branch stays reachable
// even when its namespace shares a name with another remote. Without --remote,
// a leading "<remote>/" selects that remote, matched against the longest
// configured name because remote names may contain '/'; anything else is a
// branch on origin.
func parseFetchTarget(from, remote string, remotes []string) (fetchTarget, error) {
	if strings.HasPrefix(from, "-") {
		return fetchTarget{}, fmt.Errorf("--from value %q is not a valid git ref: refs must not start with '-'", from)
	}
	if strings.HasPrefix(from, "refs/") {
		return fetchTarget{}, fmt.Errorf("--fetch needs --from to be a short branch name such as main or origin/main, got %q", from)
	}
	var target fetchTarget
	if remote != "" {
		target = fetchTarget{remote: remote, branch: strings.TrimPrefix(from, remote+"/")}
	} else {
		target = fetchTarget{remote: "origin", branch: from}
		prefix := ""
		for _, r := range remotes {
			if len(r) > len(prefix) && strings.HasPrefix(from, r+"/") {
				prefix = r
			}
		}
		if prefix != "" {
			target = fetchTarget{remote: prefix, branch: strings.TrimPrefix(from, prefix+"/")}
		}
	}
	if !slices.Contains(remotes, target.remote) {
		return fetchTarget{}, fmt.Errorf("--fetch: %q is not a configured git remote", target.remote)
	}
	if target.branch == "HEAD" {
		return fetchTarget{}, notABranchError(from)
	}
	return target, nil
}

func notABranchError(from string) error {
	return fmt.Errorf("--fetch needs --from to name a branch, got %q", from)
}

// fetchReviewBase implements --fetch: it refreshes the --from branch from its
// remote, then freezes the range endpoints so the rest of the review reads the
// commits resolved here even if the refs move again. It rewrites opts.from to
// the remote-tracking branch, which is what the session records as the
// requested base. Without --fetch it does nothing and returns nil.
//
// It runs before validateReviewRefs because the --from branch may not exist
// locally until it has been fetched, so it applies the same guards itself.
func fetchReviewBase(ctx context.Context, cc *commonContext, opts *reviewOptions) (*diff.InputResolution, error) {
	if !opts.fetch {
		return nil, nil
	}
	if err := validateReviewRefs(cc.RepoDir, reviewOptions{to: opts.to}); err != nil {
		return nil, err
	}
	remotes, err := cc.GitRunner.Output(ctx, cc.RepoDir, "remote")
	if err != nil {
		return nil, fmt.Errorf("--fetch: list git remotes: %w", err)
	}
	target, err := parseFetchTarget(opts.from, opts.remote, strings.Fields(string(remotes)))
	if err != nil {
		return nil, err
	}
	dest := target.trackingRef()
	if _, err := cc.GitRunner.Output(ctx, cc.RepoDir, "check-ref-format", dest); err != nil {
		return nil, notABranchError(opts.from)
	}
	// Fetching into a symbolic ref writes through to its target, which may be a
	// local branch.
	if _, err := cc.GitRunner.Output(ctx, cc.RepoDir, "symbolic-ref", "--quiet", dest); err == nil {
		return nil, fmt.Errorf("--fetch: %s is a symbolic ref; refusing to fetch into it", dest)
	}

	q := newQuietHandle(opts.outputFormat, opts.audience)
	defer q.Restore()
	w := stdout.Writer()
	fmt.Fprintf(w, "[ocr] Fetching %s from %s\n", target.branch, target.remote)
	// --refmap= keeps configured fetch refspecs from writing refs beyond dest,
	// and an empty fetch.bundleURI keeps bundle downloads out of refs/bundles/;
	// the other switches keep tags, submodules, FETCH_HEAD and background
	// maintenance out of it. --porcelain --verbose reports what dest now holds.
	out, stderr, err := cc.GitRunner.RunSplit(ctx, cc.RepoDir, "-c", "fetch.bundleURI=", "fetch",
		"--porcelain", "--verbose", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head",
		"--no-auto-maintenance", "--refmap=", "--end-of-options", target.remote, "+refs/heads/"+target.branch+":"+dest)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fetchFailure(target, stderr, err)
	}
	// Git exits 0 when it refuses an update, e.g. one that would need new
	// shallow roots; dest is then stale and must not be reviewed.
	if !fetchedDestination(out, dest) {
		msg := fmt.Sprintf("fetch %s from %s: git did not update %s", target.branch, target.remote, dest)
		if diag := sanitizeTerminal(strings.TrimSpace(stderr)); diag != "" {
			msg += ": " + diag
		}
		return nil, errors.New(msg)
	}
	tipOut, err := cc.GitRunner.Output(ctx, cc.RepoDir, "rev-parse", dest+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("fetch %s from %s: read %s after fetch: %w", target.branch, target.remote, dest, err)
	}
	tip := strings.TrimSpace(string(tipOut))
	base := target.remote + "/" + target.branch
	sealed, err := agent.ResolveInput(ctx, agent.Args{
		RepoDir:   cc.RepoDir,
		From:      tip,
		To:        opts.to,
		GitRunner: cc.GitRunner,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, resolveRangeError(ctx, cc, base, opts.to, err)
	}
	opts.from = base
	fmt.Fprintf(w, "[ocr] Resolved base: %s -> %s\n", base, shortSHA(tip))
	fmt.Fprintf(w, "[ocr] Resolved target: %s -> %s\n", opts.to, shortSHA(sealed.ResolvedHead))
	fmt.Fprintf(w, "[ocr] Merge base: %s\n", shortSHA(sealed.ResolvedBase))
	return sealed, nil
}

// fetchedDestination reports whether fetch porcelain output confirms dest was
// accepted. It supports both OID and arrow forms, and treats '!' as a refusal.
func fetchedDestination(porcelain, dest string) bool {
	shortDest := strings.TrimPrefix(dest, "refs/remotes/")
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 2 {
			continue
		}
		flag := rune(line[0])
		if flag == '!' {
			if fetchDestination(line[1:]) == dest || fetchDestination(line[1:]) == shortDest {
				return false
			}
			continue
		}
		if !strings.ContainsRune(" +*=t-", flag) {
			continue
		}
		gotDest := fetchDestination(line[1:])
		if gotDest == dest || gotDest == shortDest {
			return true
		}
	}
	return false
}

func fetchDestination(line string) string {
	rest := strings.TrimSpace(line)
	if rest == "" {
		return ""
	}
	if idx := strings.LastIndex(rest, "->"); idx >= 0 {
		rhs := strings.Fields(strings.TrimSpace(rest[idx+2:]))
		if len(rhs) > 0 {
			return rhs[0]
		}
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func fetchFailure(t fetchTarget, stderr string, err error) error {
	diag := sanitizeTerminal(strings.TrimSpace(stderr))
	if diag == "" {
		return fmt.Errorf("fetch %s from %s: %w", t.branch, t.remote, err)
	}
	if strings.Contains(diag, "couldn't find remote ref") {
		diag += " (--fetch refreshes a branch; to review a tag or commit, drop --fetch)"
	}
	return fmt.Errorf("fetch %s from %s: %w: %s", t.branch, t.remote, err, diag)
}

func resolveRangeError(ctx context.Context, cc *commonContext, base, to string, err error) error {
	err = fmt.Errorf("--fetch: resolve %s..%s: %w", base, to, err)
	if out, _ := cc.GitRunner.Output(ctx, cc.RepoDir, "rev-parse", "--is-shallow-repository"); strings.TrimSpace(string(out)) == "true" {
		err = fmt.Errorf("%w; this clone is shallow, so deepen it (git fetch --deepen=<n> or --unshallow) and retry", err)
	}
	return err
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

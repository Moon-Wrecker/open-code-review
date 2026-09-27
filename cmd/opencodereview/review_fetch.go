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

// parseFetchTarget reads --from as a branch on a configured remote. A leading
// "<remote>/" selects that remote, matched against the longest configured name
// because remote names may contain '/'; anything else is a branch on --remote,
// or origin. A branch literally named "origin/topic" therefore cannot be
// fetched from origin this way.
func parseFetchTarget(from, remote string, remotes []string) (fetchTarget, error) {
	if strings.HasPrefix(from, "-") {
		return fetchTarget{}, fmt.Errorf("--from value %q is not a valid git ref: refs must not start with '-'", from)
	}
	target := fetchTarget{remote: remote, branch: from}
	prefix := ""
	for _, r := range remotes {
		if len(r) > len(prefix) && strings.HasPrefix(from, r+"/") {
			prefix = r
		}
	}
	if prefix != "" {
		if remote != "" && remote != prefix {
			return fetchTarget{}, fmt.Errorf("--from %q names remote %q, but --remote is %q", from, prefix, remote)
		}
		target = fetchTarget{remote: prefix, branch: strings.TrimPrefix(from, prefix+"/")}
	}
	if target.remote == "" {
		target.remote = "origin"
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
	// --refmap= keeps configured fetch refspecs from writing refs beyond dest;
	// the other switches keep tags, submodules, FETCH_HEAD and background
	// maintenance out of it.
	_, stderr, err := cc.GitRunner.RunSplit(ctx, cc.RepoDir, "fetch", "--quiet", "--no-tags",
		"--no-recurse-submodules", "--no-write-fetch-head", "--no-auto-maintenance", "--refmap=",
		"--end-of-options", target.remote, "+refs/heads/"+target.branch+":"+dest)
	if err != nil {
		return nil, fetchFailure(target, stderr, err)
	}
	tip, err := cc.GitRunner.Output(ctx, cc.RepoDir, "rev-parse", "--verify", "--quiet", "--end-of-options", dest+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("--fetch: resolve %s: %w", dest, err)
	}
	base := target.remote + "/" + target.branch
	sealed, err := agent.ResolveInput(ctx, agent.Args{
		RepoDir:   cc.RepoDir,
		From:      strings.TrimSpace(string(tip)),
		To:        opts.to,
		GitRunner: cc.GitRunner,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, noMergeBaseError(ctx, cc, base, opts.to)
	}
	opts.from = base
	fmt.Fprintf(w, "[ocr] Resolved base: %s -> %s\n", base, shortSHA(strings.TrimSpace(string(tip))))
	fmt.Fprintf(w, "[ocr] Resolved target: %s -> %s\n", opts.to, shortSHA(sealed.ResolvedHead))
	fmt.Fprintf(w, "[ocr] Merge base: %s\n", shortSHA(sealed.ResolvedBase))
	return sealed, nil
}

func fetchFailure(t fetchTarget, stderr string, err error) error {
	msg := sanitizeTerminal(strings.TrimSpace(stderr))
	if msg == "" {
		msg = err.Error()
	}
	if strings.Contains(msg, "couldn't find remote ref") {
		msg += " (--fetch refreshes a branch; to review a tag or commit, drop --fetch)"
	}
	return fmt.Errorf("fetch %s from %s: %s", t.branch, t.remote, msg)
}

func noMergeBaseError(ctx context.Context, cc *commonContext, base, to string) error {
	msg := fmt.Sprintf("--fetch: no merge base between %s and %s", base, to)
	if out, _ := cc.GitRunner.Output(ctx, cc.RepoDir, "rev-parse", "--is-shallow-repository"); strings.TrimSpace(string(out)) == "true" {
		msg += "; this clone is shallow, so deepen it (git fetch --deepen=<n> or --unshallow) and retry"
	}
	return errors.New(msg)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

package events

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Kinds of events.
const (
	KindPush          = "push"
	KindBranchCreated = "branch-created"
	KindBranchDeleted = "branch-deleted"
	KindRepoCreated   = "repo-created"
	KindRepoDeleted   = "repo-deleted"
	KindPROpened      = "pr-opened"
	KindPRUpdated     = "pr-updated"
	KindPRMerged      = "pr-merged"
	KindPRClosed      = "pr-closed"
	KindPRReopened    = "pr-reopened"
	KindApproved      = "review-approved"
	KindRejected      = "review-rejected"
	KindPipeline      = "pipeline"
	KindApprovalCheck = "approval-check"
	KindDeploy        = "deploy"
	KindUndeploy      = "undeploy"
	KindRestore       = "restore"
	KindRoutine       = "routine"
)

// ApprovalContext is the commit status the gateway's approval check posts.
const ApprovalContext = "homecloud/approval"

type giteaRepo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (r giteaRepo) split() (ns, repo string) {
	if ns, repo, ok := strings.Cut(r.FullName, "/"); ok {
		return ns, repo
	}
	return r.Owner.Login, r.Name
}

type giteaUser struct {
	Login string `json:"login"`
}

// FromGitea turns one webhook delivery of Gitea (its X-Gitea-Event, its
// X-Gitea-Delivery and its body) into events; nil for what the feed leaves
// out. The delivery ID is the key, so a redelivery changes nothing.
func FromGitea(event, delivery string, body []byte) []Event {
	var p struct {
		Action     string    `json:"action"`
		Ref        string    `json:"ref"`
		RefType    string    `json:"ref_type"`
		After      string    `json:"after"`
		CompareURL string    `json:"compare_url"`
		Repository giteaRepo `json:"repository"`
		Sender     giteaUser `json:"sender"`
		Pusher     giteaUser `json:"pusher"`
		Commits    []struct {
			Message string `json:"message"`
		} `json:"commits"`
		TotalCommits int `json:"total_commits"`
		PullRequest  *struct {
			Number         int       `json:"number"`
			Title          string    `json:"title"`
			HTMLURL        string    `json:"html_url"`
			Merged         bool      `json:"merged"`
			MergeCommitSHA string    `json:"merge_commit_sha"`
			User           giteaUser `json:"user"`
			Head           struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
		Review *struct {
			Content string `json:"content"`
		} `json:"review"`
		// status
		SHA         string `json:"sha"`
		Context     string `json:"context"`
		State       string `json:"state"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
		// workflow_run
		WorkflowRun *struct {
			ID           int64  `json:"id"`
			Name         string `json:"name"`
			DisplayTitle string `json:"display_title"`
			HeadBranch   string `json:"head_branch"`
			HeadSHA      string `json:"head_sha"`
			Status       string `json:"status"`
			Conclusion   string `json:"conclusion"`
			HTMLURL      string `json:"html_url"`
			Event        string `json:"event"`
		} `json:"workflow_run"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	ns, repo := p.Repository.split()
	if ns == "" {
		return nil
	}
	e := Event{Source: SourceGitea, Namespace: ns, Repo: repo, Actor: p.Sender.Login, Status: StatusInfo, Link: p.Repository.HTMLURL}
	if delivery != "" {
		e.Key = "gitea:" + delivery
	}
	switch event {
	case "push":
		branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
		if !ok || strings.Trim(p.After, "0") == "" {
			return nil // tags, and deleted branches (a delete event says so)
		}
		n := p.TotalCommits
		if n == 0 {
			n = len(p.Commits)
		}
		if p.Pusher.Login != "" {
			e.Actor = p.Pusher.Login
		}
		e.Kind, e.Commit, e.Link = KindPush, p.After, p.CompareURL
		e.Title = fmt.Sprintf("%s pushed %s to %s", e.Actor, plural(n, "commit"), branch)
		if len(p.Commits) > 0 {
			e.Detail = firstLine(p.Commits[len(p.Commits)-1].Message)
		}
	case "create", "delete":
		if p.RefType != "branch" {
			return nil
		}
		e.Kind = map[string]string{"create": KindBranchCreated, "delete": KindBranchDeleted}[event]
		e.Title = fmt.Sprintf("%s %sd the branch %s", e.Actor, event, strings.TrimPrefix(p.Ref, "refs/heads/"))
	case "repository":
		switch p.Action {
		case "created":
			e.Kind = KindRepoCreated
		case "deleted":
			e.Kind = KindRepoDeleted
		default:
			return nil
		}
		e.Title = fmt.Sprintf("%s %s the repo %s/%s", e.Actor, p.Action, ns, repo)
	case "pull_request", "pull_request_sync", "pull_request_review_approved", "pull_request_review_rejected":
		pr := p.PullRequest
		if pr == nil {
			return nil
		}
		e.PR, e.Link, e.Commit = pr.Number, pr.HTMLURL, pr.Head.SHA
		what := fmt.Sprintf("#%d %s", pr.Number, pr.Title)
		switch {
		case event == "pull_request_review_approved":
			e.Kind, e.Title, e.Status = KindApproved, e.Actor+" approved "+what, StatusOK
			if p.Review != nil {
				e.Detail = p.Review.Content
			}
		case event == "pull_request_review_rejected":
			e.Kind, e.Title = KindRejected, e.Actor+" asked for changes to "+what
			if p.Review != nil {
				e.Detail = p.Review.Content
			}
		case p.Action == "opened":
			e.Kind, e.Title = KindPROpened, fmt.Sprintf("%s proposed %s (%s → %s)", pr.User.Login, what, pr.Head.Ref, pr.Base.Ref)
			e.Actor = pr.User.Login
		case p.Action == "synchronized" || event == "pull_request_sync":
			e.Kind, e.Title = KindPRUpdated, "new commits on "+what
		case p.Action == "closed" && pr.Merged:
			e.Kind, e.Title, e.Status = KindPRMerged, fmt.Sprintf("%s merged %s into %s", e.Actor, what, pr.Base.Ref), StatusOK
			if pr.MergeCommitSHA != "" {
				e.Commit = pr.MergeCommitSHA
			}
		case p.Action == "closed":
			e.Kind, e.Title = KindPRClosed, e.Actor+" closed "+what
		case p.Action == "reopened":
			e.Kind, e.Title = KindPRReopened, e.Actor+" reopened "+what
		default:
			return nil
		}
	case "status":
		// Pipelines come as workflow runs; of the statuses, the feed keeps
		// the approval check's verdicts.
		if p.Context != ApprovalContext || (p.State != "success" && p.State != "failure" && p.State != "error") {
			return nil
		}
		e.Kind, e.Commit, e.Link, e.Actor = KindApprovalCheck, p.SHA, p.TargetURL, ""
		e.Title, e.Status = "approval check: "+p.Description, StatusOK
		if p.State != "success" {
			e.Status = StatusRefused
		}
		// One verdict per commit and outcome, however often it is posted.
		e.Key = "approval:" + ns + "/" + repo + "@" + p.SHA + ":" + p.State + ":" + p.Description
	case "workflow_run":
		run := p.WorkflowRun
		if run == nil || p.Action != "completed" {
			return nil
		}
		name := run.DisplayTitle
		if name == "" {
			name = run.Name
		}
		e.Kind, e.Commit, e.Link = KindPipeline, run.HeadSHA, run.HTMLURL
		switch run.Conclusion {
		case "success":
			e.Status = StatusOK
		case "skipped", "cancelled", "neutral":
			e.Status = StatusInfo
		default:
			e.Status = StatusFailed
		}
		e.Title = fmt.Sprintf("pipeline on %s: %s (%s)", run.HeadBranch, run.Conclusion, name)
		e.Key = fmt.Sprintf("run:%s/%s:%d", ns, repo, run.ID)
	default:
		return nil
	}
	return []Event{e}
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

package source

import (
	"context"
	"time"
)

type Repo struct {
	Owner string
	Name  string
}

func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

type User struct {
	Login     string
	AvatarURL string
	HTMLURL   string
}

type Issue struct {
	Number    int64
	Title     string
	Body      string
	State     string
	Labels    []string
	Author    User
	HTMLURL   string
	CreatedAt time.Time
	UpdatedAt time.Time
	ClosedAt  *time.Time
}

type PullRequest struct {
	Issue

	HeadRepo     Repo
	HeadBranch   string
	HeadSHA      string
	HeadCloneURL string

	BaseRepo   Repo
	BaseBranch string

	Merged   bool
	MergedAt *time.Time
}

type Comment struct {
	ID          int64
	IssueNumber int64
	Body        string
	Author      User
	HTMLURL     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type ListOpts struct {
	Since time.Time
}

type Provider interface {
	Kind() string
	Host() string
	ListIssues(ctx context.Context, repo Repo, opts ListOpts) ([]Issue, error)
	ListPullRequests(ctx context.Context, repo Repo, opts ListOpts) ([]PullRequest, error)
	ListComments(ctx context.Context, repo Repo, issueNumber int64, opts ListOpts) ([]Comment, error)
}

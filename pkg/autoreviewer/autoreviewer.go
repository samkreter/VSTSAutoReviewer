package autoreviewer

import (
	"context"
	"fmt"
	"strings"

	adogit "github.com/microsoft/azure-devops-go-api/azuredevops/git"
	adoidentity "github.com/microsoft/azure-devops-go-api/azuredevops/identity"
	"github.com/samkreter/go-core/log"

	"github.com/samkreter/devopshelper/pkg/utils"
	"github.com/samkreter/devopshelper/pkg/store"
	"github.com/samkreter/devopshelper/pkg/types"
)

const (
	defaultBotIdentifier = "b03f5f7f11d50a3a"
)

var (
	defaultFilters = []Filter{
		filterWIP,
		filterMasterBranchOnly,
	}
)

// Filter is a function returns true if a pull request should be filtered out.
type Filter func(*PullRequest) bool

// ReviewerTrigger is called with the reviewers that have been selected. Allows for adding custom events
//  for each reviewer that is added to the PR. Ex: slack notification.
type ReviewerTrigger func([]*types.Reviewer, string) error


// AutoReviewer automaticly adds reviewers to a vsts pull request
type AutoReviewer struct {
	filters          []Filter
	reviewerTriggers []ReviewerTrigger
	adoGitClient     adogit.Client
	adoIdentityClient adoidentity.Client
	botIdentifier         string
	Repo             *types.Repository
	RepoStore        store.RepositoryStore
}

// ReviewerInfo describes who to be added as a reviwer and which files to watch for
type ReviewerInfo struct {
	File           string   `json:"file"`
	ActivePaths    []string `json:"activePaths"`
	reviewerGroups types.ReviewerGroups
}

// NewAutoReviewer creates a new autoreviewer
func NewAutoReviewer(adoGitClient adogit.Client, adoIdentityClient adoidentity.Client, botIdentifier string, repo *types.Repository, repoStore store.RepositoryStore, filters []Filter, rTriggers []ReviewerTrigger) (*AutoReviewer, error) {
	return &AutoReviewer{
		Repo:              repo,
		RepoStore:         repoStore,
		adoGitClient:      adoGitClient,
		adoIdentityClient: adoIdentityClient,
		filters:           filters,
		reviewerTriggers:  rTriggers,
		botIdentifier:     botIdentifier,
	}, nil
}

// Run starts the autoreviewer for a single instance
func (a *AutoReviewer) Run(ctx context.Context) error {
	logger := log.G(ctx)

	if err := a.ensureRepo(ctx); err != nil {
		return err
	}

	pullRequests, err := a.adoGitClient.GetPullRequests(ctx, adogit.GetPullRequestsArgs{
		RepositoryId: &a.Repo.AdoRepoID,
		Project: &a.Repo.ProjectName,
		SearchCriteria: &adogit.GitPullRequestSearchCriteria{},
	})
	if err != nil {
		return fmt.Errorf("get pull requests error: %v", err)
	}

	for _, pr := range *pullRequests {
		pullRequest := &PullRequest{pr}

		if a.shouldFilter(pullRequest) {
			continue
		}

		if err := a.balanceReview(ctx, pullRequest); err != nil {
			logger.Errorf("ERROR: balancing reviewers with error %v", err)
		}
	}

	return nil
}

func (a *AutoReviewer) ensureRepo(ctx context.Context) error {
	if err := a.ensureAdoRepoID(ctx); err != nil {
		return err
	}

	if err := a.ensureReviewersIDs(ctx); err != nil {
		return err
	}

	return nil
}

func (a *AutoReviewer) ensureReviewersIDs(ctx context.Context) error {
	updated := false
	for _, reviewerGroup := range a.Repo.ReviewerGroups {
		for _, reviewer := range reviewerGroup.Reviewers {
			if reviewer.AdoID == "" {
				identity, err := utils.GetDevOpsIdentity(ctx, reviewer.Alias, a.adoIdentityClient)
				if err != nil {
					return err
				}

				reviewer.AdoID = identity.Id.String()
				updated = true
			}
		}
	}

	if updated {
		if err := a.RepoStore.UpdateRepository(ctx, a.Repo.ID.String(), a.Repo); err != nil {
			return err
		}
	}

	return nil
}

func (a *AutoReviewer) ensureAdoRepoID(ctx context.Context) error{
	if a.Repo.AdoRepoID != "" {
		return nil
	}

	adoRepos, err := a.adoGitClient.GetRepositories(ctx, adogit.GetRepositoriesArgs{
		Project: &a.Repo.ProjectName,
	})
	if err != nil {
		return err
	}

	for _, adoRepo := range *adoRepos {
		if *adoRepo.Name == a.Repo.Name {
			a.Repo.AdoRepoID = adoRepo.Id.String()
			if err := a.RepoStore.UpdateRepository(ctx, a.Repo.ID.Hex(), a.Repo); err != nil {
				return err
			}
			return nil
		}
	}

	return fmt.Errorf("repo: %s not found in project %s", a.Repo.Name, a.Repo.ProjectName)
}

func (a *AutoReviewer) shouldFilter(pr *PullRequest) bool {
	for _, filter := range a.filters {
		if filter(pr) {
			return true
		}
	}

	return false
}

func (a *AutoReviewer) getReviewers(ctx context.Context, pr *PullRequest) error {
	reviewerGroups, err := pr.GetReviewerGroups(ctx, a.adoGitClient)
	if err != nil {
		return err
	}

	// Get all required owners
	owners := map[string]bool{}
	groups := map[string]bool{}
	for _, reviewerGroup := range reviewerGroups {
		groups[reviewerGroup.Group] = true

		for owner := range reviewerGroup.Owners {
			owners[owner] = true
		}
	}
	return nil
}

func (a *AutoReviewer) balanceReview(ctx context.Context, pr *PullRequest) error {
	logger := log.G(ctx)

	if a.ContainsReviewBalancerComment(ctx, pr.Repository.Id.String(),  *pr.PullRequestId) {
		return nil
	}

	requiredReviewers, optionalReviewers, err := a.Repo.ReviewerGroups.GetReviewers(*pr.CreatedBy.Id)
	if err != nil {
		return err
	}

	// save the repo after pos change
	if err := a.RepoStore.UpdateRepository(ctx, a.Repo.ID.Hex(), a.Repo); err != nil {
		return err
	}

	if err := a.AddReviewers(ctx, *pr.PullRequestId, pr.Repository.Id.String(), requiredReviewers, optionalReviewers); err != nil {
		return fmt.Errorf("add reviewers error: %v", err)
	}

	comment := fmt.Sprintf(
		"Hello %s,\r\n\r\n"+
			"You are randomly selected as the **required** code reviewers of this change. \r\n\r\n"+
			"Your responsibility is to review **each** iteration of this CR until signoff. You should provide no more than 48 hour SLA for each iteration.\r\n\r\n"+
			"Thank you.\r\n\r\n"+
			"CR Balancer\r\n"+
			"%s",
		strings.Join(GetReviewersAlias(requiredReviewers), ","),
		a.botIdentifier)

	repoID := pr.Repository.Id.String()
	_, err = a.adoGitClient.CreateThread(ctx, adogit.CreateThreadArgs{
		RepositoryId: &repoID,
		PullRequestId: pr.PullRequestId,
		CommentThread: &adogit.GitPullRequestCommentThread{
			Comments: &[]adogit.Comment{
				{
					Content: &comment,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("add thread error: %v", err)
	}

	logger.Infof("Adding %s as required reviewers and %s as observer to PR: %d",
		GetReviewersAlias(requiredReviewers),
		GetReviewersAlias(requiredReviewers),
		*pr.PullRequestId)

	for _, rTrigger := range a.reviewerTriggers {
		if err := rTrigger(requiredReviewers, *pr.Url); err != nil {
			logger.Error(err)
		}
	}

	return nil
}

// ContainsReviewBalancerComment checks if the passed in review has had a bot comment added.
func (a *AutoReviewer) ContainsReviewBalancerComment(ctx context.Context, repositoryID string, pullRequestID int) bool {
	threads, err := a.adoGitClient.GetThreads(ctx, adogit.GetThreadsArgs{
		RepositoryId: &repositoryID,
		PullRequestId: &pullRequestID,
	})
	if err != nil {
		panic(err)
	}

	if threads != nil {
		for _, thread := range *threads {
			for _, comment := range *thread.Comments {
				if strings.Contains(*comment.Content, a.botIdentifier) {
					return true
				}
			}
		}
	}
	return false
}

// AddReviewers adds the passing in reviewers to the pull requests for the passed in review.
func (a *AutoReviewer) AddReviewers(ctx context.Context, pullRequestID int, repoID string, required, optional []*types.Reviewer) error {
	for _, reviewer := range append(required, optional...) {
		_, err := a.adoGitClient.CreatePullRequestReviewer(ctx, adogit.CreatePullRequestReviewerArgs{
			Reviewer: &adogit.IdentityRefWithVote{},
			ReviewerId: &reviewer.AdoID,
			RepositoryId: &repoID,
			PullRequestId: &pullRequestID,
		})
		if err != nil {
			return fmt.Errorf("failed to add reviewer %s with error %v", reviewer.Alias, err)
		}
	}

	return nil
}

// GetReviewersAlias gets all names for the set of passed in reviewers
// return: string slice of the aliases
func GetReviewersAlias(reviewers []*types.Reviewer) []string {
	aliases := make([]string, len(reviewers))

	for index, reviewer := range reviewers {
		aliases[index] = reviewer.Alias
	}
	return aliases
}

func filterWIP(pr *PullRequest) bool {
	if strings.Contains(*pr.Title, "WIP") {
		return true
	}

	return false
}

func filterMasterBranchOnly(pr *PullRequest) bool {
	if strings.EqualFold(*pr.TargetRefName, "refs/heads/master") {
		return false
	}

	return true
}

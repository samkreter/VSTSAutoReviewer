package cmd

import (
	"fmt"
	"strings"

	"github.com/samkreter/VSTSAutoReviewer/vsts"
)

func Run() error {
	reviews := vsts.GetInprogressReviews()

	for _, review := range reviews {
		balanceReview(review)
	}

	return nil
}

func balanceReview(reviewSummary vsts.ReviewSummary) {
	if !vsts.ContainsReviewBalancerComment(reviewSummary) {

		//need reiew iteration algo to change reviewers
		requiredReviewers, optionalReviewers := vsts.LoadReviewers()

		vsts.AddReviewers(reviewSummary, requiredReviewers, optionalReviewers)

		comment := fmt.Sprintf(
			"Hello %s,\r\n\r\n"+
				"You are randomly selected as the **required** code reviewers of this change. \r\n\r\n"+
				"Your responsibility is to review **each** iteration of this CR until signoff. You should provide no more than 48 hour SLA for each iteration.\r\n\r\n"+
				"Thank you.\r\n\r\n"+
				"CR Balancer\r\n"+
				"%s",
			strings.Join(vsts.GetReviewersAlias(requiredReviewers), ","),
			vsts.Conf.VstsBotMaker)

		vsts.AddRootComment(reviewSummary, comment)
	}
}
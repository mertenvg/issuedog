package github

import (
	"errors"
	"net/http"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
)

// This file is #409, after #245's revert of the issue type by the name the
// org gives it now. A type the org no longer lists is left out for the
// type that stands, with the revert it blocks (IssueTypeLeftOut.Gone),
// which the daemon reports once as a state error. A refused read of the
// org's list, a 403 or a 404, keeps the timeline's name, as before #245,
// so a revert with no rename still goes through. And a type's name, the
// org's text, is fenced as code that reads as it is (TypeGoneCodeSpan).

// errTypeGoneRefused is wrapped by IssueTypeNames' error when GitHub
// refused the read of the org's issue types: a 403, as without the
// organization permission Issue Types read (SPEC §15), or a 404, which
// OrgIssueTypes gives as no org, as an org the App can't see. Either says
// nothing of the org's types, so no type counts as gone for it, and a
// revert keeps the timeline's name (typeRenameCurrent).
var errTypeGoneRefused = errors.New("GitHub refused the read")

// typeGoneRefusal reports whether err, OrgIssueTypes', is GitHub's HTTP
// 403. go-github gives a rate limit's 403 its own types, so that's never
// one: it fails the read, to try again on the next pass.
func typeGoneRefusal(err error) bool {
	var er *gogithub.ErrorResponse
	return errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusForbidden
}

// TypeGoneCodeSpan is s as an inline code span: fenced by one more
// backtick than its longest run of them, with a space inside each fence if
// s starts or ends with a backtick or a space, which CommonMark strips, so
// it reads as it is. A code span links no mention and makes no HTML. An
// empty s is one space. s is one line, and the caller escapes its markers
// (Escape), before or after: neither changes a backtick.
func TypeGoneCodeSpan(s string) string {
	if s == "" {
		return "` `"
	}
	longest, run := 0, 0
	for _, r := range s {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	fence, pad := strings.Repeat("`", longest+1), ""
	if strings.ContainsAny(s[:1], "` ") || strings.ContainsAny(s[len(s)-1:], "` ") {
		pad = " "
	}
	return fence + pad + s + pad + fence
}

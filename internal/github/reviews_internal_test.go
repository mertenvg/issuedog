package github

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

// A review comment's line is its line, or else, for an outdated comment,
// which GitHub serves with a null line, its original_line, or 0 for a
// comment on the whole file, which has neither.
func TestReviewCommentsLine(t *testing.T) {
	t.Parallel()
	c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/5/reviews/9/comments" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[
			{"id":1,"user":{"login":"owner"},"path":"a.go","line":12,"original_line":10,"body":"moved"},
			{"id":2,"user":{"login":"owner"},"path":"b.go","line":null,"original_line":7,"body":"outdated"},
			{"id":3,"user":{"login":"owner"},"path":"c.go","line":null,"original_line":null,"subject_type":"file","body":"whole file"}]`))
	}))
	got, err := c.ReviewComments(context.Background(), "o", "r", 5, 9)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReviewComment{{ID: 1, Author: "owner", Path: "a.go", Line: 12, Body: "moved"},
		{ID: 2, Author: "owner", Path: "b.go", Line: 7, Body: "outdated"},
		{ID: 3, Author: "owner", Path: "c.go", Body: "whole file"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("comments %+v; want %+v", got, want)
	}
}

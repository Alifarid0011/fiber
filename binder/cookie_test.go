package binder

import (
	"testing"
)

func Benchmark_CookieBinder_Bind(b *testing.B) {
	b.ReportAllocs()

	binder := &CookieBinding{EnableSplitting: true}

	type User struct {
		Name  string   `query:"name"`
		Posts []string `query:"posts"`
		Age   int      `query:"age"`
	}
	var user User

	req := acquireRequest(b)
	req.Header.SetCookie("name", "john")
	req.Header.SetCookie("age", "42")
	req.Header.SetCookie("posts", "post1,post2,post3")

	var err error
	for b.Loop() {
		err = binder.Bind(req, &user)
	}

	requireJohn42(b, err, user.Name, user.Age, user.Posts)
}

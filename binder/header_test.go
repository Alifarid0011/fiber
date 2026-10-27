package binder

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func Benchmark_HeaderBinder_Bind(b *testing.B) {
	b.ReportAllocs()

	binder := &HeaderBinding{
		EnableSplitting: true,
	}

	type User struct {
		Name  string   `header:"Name"`
		Posts []string `header:"Posts"`
		Age   int      `header:"Age"`
	}
	var user User

	req := fasthttp.AcquireRequest()
	b.Cleanup(func() { fasthttp.ReleaseRequest(req) })

	req.Header.Set("name", "john")
	req.Header.Set("age", "42")
	req.Header.Set("posts", "post1,post2,post3")

	var err error
	for b.Loop() {
		err = binder.Bind(req, &user)
	}

	require.NoError(b, err)
	require.Equal(b, "john", user.Name)
	require.Equal(b, 42, user.Age)
	require.Len(b, user.Posts, 3)
	require.Contains(b, user.Posts, "post1")
	require.Contains(b, user.Posts, "post2")
	require.Contains(b, user.Posts, "post3")
}

func Test_HeaderBinder_Bind_Splitting_StringMapNotSplit(t *testing.T) {
	t.Parallel()

	b := &HeaderBinding{
		EnableSplitting: true,
	}

	const userAgent = "Mozilla/5.0 (X11; Linux) AppleWebKit/537.36 (KHTML, like Gecko)"

	req := acquireRequest(t)
	req.Header.Set("User-Agent", userAgent)

	// A map[string]string keeps one value per key, so nothing is split.
	stringMap := make(map[string]string)
	require.NoError(t, b.Bind(req, &stringMap))
	require.Equal(t, userAgent, stringMap["User-Agent"])

	// A map[string][]string holds every piece.
	sliceMap := make(map[string][]string)
	require.NoError(t, b.Bind(req, &sliceMap))
	require.Equal(t, []string{"Mozilla/5.0 (X11; Linux) AppleWebKit/537.36 (KHTML", " like Gecko)"}, sliceMap["User-Agent"])
}

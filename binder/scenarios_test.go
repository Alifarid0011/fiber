package binder

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// pairUser is what the scenarios below bind into. Each binder finds the fields
// through its own tag.
type pairUser struct {
	Name  string   `cookie:"name" form:"name" header:"Name" query:"name" respHeader:"name"`
	Names []string `cookie:"names" form:"names" header:"Names" query:"names" respHeader:"names"`
	Posts []string `cookie:"posts" form:"posts" header:"Posts" query:"posts" respHeader:"posts"`
	Age   int      `cookie:"age" form:"age" header:"Age" query:"age" respHeader:"age"`
}

// Test_PairSources_Bind pins that every binder that reads name/value pairs
// fills a struct from them, and splits a comma separated value into a slice
// when splitting is on.
func Test_PairSources_Bind(t *testing.T) {
	t.Parallel()

	for _, src := range pairSources {
		t.Run(src.name, func(t *testing.T) {
			t.Parallel()

			var user pairUser
			err := src.bind(t, "name=john&names=john,doe&age=42&posts=post1,post2,post3", true, &user)

			require.NoError(t, err)
			require.Equal(t, "john", user.Name)
			require.Equal(t, 42, user.Age)
			require.Equal(t, []string{"post1", "post2", "post3"}, user.Posts)
			require.Contains(t, user.Names, "john")
			require.Contains(t, user.Names, "doe")
		})
	}
}

// Test_PairSources_ParseError pins that a value the destination cannot hold is
// an error from every binder that reads name/value pairs.
func Test_PairSources_ParseError(t *testing.T) {
	t.Parallel()

	for _, src := range pairSources {
		t.Run(src.name, func(t *testing.T) {
			t.Parallel()

			var user pairUser
			require.Error(t, src.bind(t, "age=invalid", false, &user))
		})
	}
}

func Test_FormSources_Bind(t *testing.T) {
	t.Parallel()

	type Post struct {
		Title string `form:"title" query:"title"`
	}

	type User struct {
		Name  string   `form:"name" query:"name"`
		Names []string `form:"names" query:"names"`
		Posts []Post   `form:"posts" query:"posts"`
		Age   int      `form:"age" query:"age"`
	}

	for _, src := range formSources {
		t.Run(src.name, func(t *testing.T) {
			t.Parallel()

			var user User
			err := src.bind(t, "name=john&names=john,doe&age=42&posts[0][title]=post1&posts[1][title]=post2&posts[2][title]=post3", true, &user)

			require.NoError(t, err)
			require.Equal(t, "john", user.Name)
			require.Equal(t, 42, user.Age)
			require.Len(t, user.Posts, 3)
			require.Equal(t, "post1", user.Posts[0].Title)
			require.Equal(t, "post2", user.Posts[1].Title)
			require.Equal(t, "post3", user.Posts[2].Title)
			require.Contains(t, user.Names, "john")
			require.Contains(t, user.Names, "doe")
		})
	}
}

func Test_FormSources_PointerScalars(t *testing.T) {
	t.Parallel()

	type Scalars struct {
		ID     *int64   `form:"id" query:"id"`
		Name   *string  `form:"name" query:"name"`
		Active *bool    `form:"active" query:"active"`
		Score  *float64 `form:"score" query:"score"`
	}

	runFormSources(t, []formCase[Scalars]{
		{
			name:  "all fields provided",
			input: "id=123&name=test&active=true&score=98.5",
			want:  Scalars{ID: new(int64(123)), Name: new("test"), Active: new(true), Score: new(98.5)},
		},
		{
			name:  "no fields provided",
			input: "",
			want:  Scalars{},
		},
		{
			name:  "partial fields provided",
			input: "id=456&active=false",
			want:  Scalars{ID: new(int64(456)), Active: new(false)},
		},
		{
			name:  "explicit zero and empty values provided",
			input: "id=0&name=&active=false&score=0",
			want:  Scalars{ID: new(int64(0)), Name: new(""), Active: new(false), Score: new(0.0)},
		},
	})
}

func Test_FormSources_OptionalIDParam(t *testing.T) {
	t.Parallel()

	// Use case from original issue
	type OptionalIDParam struct {
		IDPtr *int64 `form:"id" query:"id"`
	}

	runFormSources(t, []formCase[OptionalIDParam]{
		{name: "id provided", input: "id=123", want: OptionalIDParam{IDPtr: new(int64(123))}},
		{name: "id not provided", input: "", want: OptionalIDParam{}},
		{name: "id zero", input: "id=0", want: OptionalIDParam{IDPtr: new(int64(0))}},
	})
}

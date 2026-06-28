package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateName_Valid(t *testing.T) {
	cases := []string{
		"hello",
		"my-job",
		"my.job",
		"a",
		"a1",
		"a-b.c-d",
		"123",
		strings.Repeat("a", 253),
	}
	for _, name := range cases {
		err := ValidateName(name)
		assert.NoError(t, err, "name %q should be valid", name)
	}
}

func TestValidateName_Invalid(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"", "is required"},
		{"my/job", "is invalid"},
		{"MyJob", "is invalid"},
		{"my_job", "is invalid"},
		{"my job", "is invalid"},
		{"-myjob", "is invalid"},
		{"myjob-", "is invalid"},
		{".myjob", "is invalid"},
		{"myjob.", "is invalid"},
		{strings.Repeat("a", 254), "is invalid"},
	}
	for _, c := range cases {
		err := ValidateName(c.name)
		require.Error(t, err, "name %q should be invalid", c.name)
		assert.Contains(t, err.Error(), c.want, "name %q", c.name)
	}
}

package agent

import "context"

// TokenSource returns the current bearer access token for an agent request.
// Implementations may refresh the token before returning it.
type TokenSource interface {
	Token(context.Context) (string, error)
}

type staticTokenSource string

func (s staticTokenSource) Token(context.Context) (string, error) {
	return string(s), nil
}

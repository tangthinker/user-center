package auth

type Auth interface {
	Sign(uid string) (string, error)
	Verify(token string) (string, error)
}

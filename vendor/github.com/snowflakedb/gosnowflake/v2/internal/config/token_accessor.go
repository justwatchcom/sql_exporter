package config

// TokenAccessor manages the session token and master token
type TokenAccessor interface {
	GetTokens() (token string, masterToken string, sessionID int64)
	SetTokens(token string, masterToken string, sessionID int64)
	Lock() error
	Unlock()
}

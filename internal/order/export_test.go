package order

// IdempotencyLockKey exposes the lock key format to tests.
func IdempotencyLockKey(userID int64, idemKey string) string {
	return idempotencyLockKey(userID, idemKey)
}

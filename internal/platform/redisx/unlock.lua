-- Delete the lock only if it still carries our token, so a request whose
-- lock already expired cannot release a lock taken by another request.
-- KEYS[1] = lock key, ARGV[1] = token
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0

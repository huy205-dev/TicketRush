-- lease.lua: take or renew the token-issuing lease of an event, so only one
-- waiting room instance admits buyers at a time (SPEC.md 7.2).
-- KEYS[1] = admit:lock:{eid}, ARGV[1] = instance id, ARGV[2] = TTL in ms
-- Returns 1 if this instance holds the lease, 0 if another one does.
local v = redis.call('GET', KEYS[1])
if v == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
if not v then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
return 0

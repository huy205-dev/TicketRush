-- claim.lua: mark an admission token used by one order (SPEC.md 9.1 step 5).
-- KEYS[1] = admission:used:<jti>, ARGV[1] = orderId, ARGV[2] = TTL in ms
-- Returns 1 if the token is now held by orderId (also when it already was),
-- 0 if another order holds it.
local v = redis.call('GET', KEYS[1])
if not v then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
if v == ARGV[1] then
  return 1
end
return 0

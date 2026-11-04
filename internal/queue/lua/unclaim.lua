-- unclaim.lua: give an admission token back, only if orderId still holds it.
-- KEYS[1] = admission:used:<jti>, ARGV[1] = orderId
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0

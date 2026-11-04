-- join.lua: put a user in an event's queue once (SPEC.md 7.2).
-- KEYS[1] = queue:{eid}             sorted set, member queueId, score join time (ms)
-- KEYS[2] = queue:{eid}:user:<uid>  the user's queueId
-- KEYS[3] = queue:{eid}:owner       hash queueId -> userId, read by the worker
-- ARGV[1] = new queueId, ARGV[2] = userId, ARGV[3] = TTL of the user key (s),
-- ARGV[4] = a queueId of this user the caller found spent (neither queued
--           nor admitted), or "".
-- Returns {queueId, rank}: rank is 0-based, or -1 when that entry has left
-- the queue because it was admitted.
local cur = redis.call('GET', KEYS[2])
if cur and cur ~= ARGV[4] then
  local r = redis.call('ZRANK', KEYS[1], cur)
  if r then
    return {cur, r}
  end
  return {cur, -1}
end
-- Redis' clock orders the queue, so every waiting room instance agrees.
local t = redis.call('TIME')
local score = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('ZADD', KEYS[1], score, ARGV[1])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[2])
redis.call('SET', KEYS[2], ARGV[1], 'EX', ARGV[3])
return {ARGV[1], redis.call('ZRANK', KEYS[1], ARGV[1])}

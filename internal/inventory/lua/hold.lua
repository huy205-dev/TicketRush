-- hold.lua (SPEC.md 6.3): hold every seat for one order, or none of them.
-- KEYS: seat keys of a single event (same hash tag, so one cluster slot)
-- ARGV[1] = orderId, ARGV[2] = ttl_ms
-- Returns {1} when every seat is held; {0, key1, key2...} with the taken seats.
-- Idempotent: running it again with the same orderId succeeds.
local taken = {}
for _, k in ipairs(KEYS) do
  local v = redis.call('GET', k)
  if v and v ~= ('held:' .. ARGV[1]) then
    table.insert(taken, k)
  end
end
if #taken > 0 then
  local res = {0}
  for _, k in ipairs(taken) do table.insert(res, k) end
  return res
end
for _, k in ipairs(KEYS) do
  redis.call('SET', k, 'held:' .. ARGV[1], 'PX', ARGV[2])
end
return {1}

-- release.lua (SPEC.md 6.4): delete a seat key only if this order holds it.
-- ARGV[1] = orderId
-- Returns the number of seats released.
local n = 0
for _, k in ipairs(KEYS) do
  if redis.call('GET', k) == ('held:' .. ARGV[1]) then
    redis.call('DEL', k)
    n = n + 1
  end
end
return n

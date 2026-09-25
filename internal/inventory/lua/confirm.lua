-- confirm.lua (SPEC.md 6.5): mark seats sold for good. Called AFTER
-- PostgreSQL has committed the tickets.
-- ARGV[1] = orderId
-- Returns {1}, or {0, key, value} for a seat owned by another order, which
-- cannot happen in a correct system; the caller logs it and reconcile fixes it.
for _, k in ipairs(KEYS) do
  local v = redis.call('GET', k)
  if v == false or v == ('held:' .. ARGV[1]) or v == ('sold:' .. ARGV[1]) then
    redis.call('SET', k, 'sold:' .. ARGV[1])
  else
    return {0, k, v}
  end
end
return {1}

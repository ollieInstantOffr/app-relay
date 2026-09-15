-- The Relay admin UI host (general.adminDomain) takes large uploads: backup
-- restores and Nginx Proxy Manager databases. Relay's API enforces its own
-- limits, so the proxy in front of it doesn't cap request bodies (the 1m
-- default cut those uploads off). Hosts with a size set by the user keep it.
UPDATE hosts
SET data = json_set(data, '$.maxBodySize', '0')
WHERE json_extract(data, '$.system') = 1
  AND COALESCE(json_extract(data, '$.maxBodySize'), '') = '';

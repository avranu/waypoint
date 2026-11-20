-- The escape ledger's due query matches a pull request to a bug task's link by repo (NOCASE) and number; without this it scans every pull request per project and times out.
CREATE INDEX pull_requests_repo_number ON pull_requests(repo COLLATE NOCASE, number);

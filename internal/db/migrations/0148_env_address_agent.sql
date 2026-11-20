-- Who last set an environment's addresses (url, health_url, version_url)
-- (WP-749, WP-ADR90 amending WP-ADR10): '' when a person did, in the web or their
-- own terminal; otherwise the agent, agent shell, automation or token that
-- did. The guard lets an unattended agent session read from the addresses
-- a person set, and from no others, so an agent can't open the guard by
-- recording an address itself.
ALTER TABLE environments ADD COLUMN address_agent TEXT NOT NULL DEFAULT '';

-- Existing rows: who set them isn't on record (created_agent is empty for a
-- person's token used from an agent's shell, and edits weren't tracked), so
-- they are unknown until a person saves them. An agent-created one keeps
-- its creator.
UPDATE environments SET address_agent = CASE
    WHEN created_agent <> '' THEN created_agent
    ELSE 'unknown' END;

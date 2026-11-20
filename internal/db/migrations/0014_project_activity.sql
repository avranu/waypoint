-- Project lists sort by recent activity: the newest issue change per project.
CREATE INDEX issues_project_updated ON issues(project_id, updated_at);

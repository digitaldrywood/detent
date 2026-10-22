create table if not exists detent_work_items (
project_id text not null,
id text not null,
identifier text not null,
title text not null default '',
description text not null default '',
priority integer,
state text not null default '' collate nocase,
url text not null default '',
author_id text not null default '',
assignee_id text not null default '',
assignees_json text not null default '[]',
labels_json text not null default '[]',
fields_json text not null default '{}',
metadata_json text not null default '{}',
deliverable_kind text not null default '',
deliverable_path text not null default '',
deliverable_review_url text not null default '',
deliverable_validation_status text not null default '',
deliverable_external_id text not null default '',
deliverable_metadata_json text not null default '{}',
assigned_to_worker integer not null default 1,
created_at text not null default '',
updated_at text not null default '',
stage_updated_at text not null default '',
model_override text not null default '',
github_node_id text not null default '',
github_repository_id integer not null default 0,
github_issue_number integer not null default 0,
github_upstream_state text not null default '',
github_orphaned integer not null default 0,
number integer not null default 0,
primary key (project_id, id)
);
create table if not exists detent_work_item_events (
id integer primary key autoincrement,
project_id text not null,
item_id text not null,
event_kind text not null,
state text not null default '',
body text not null default '',
payload_json text not null default '{}',
created_at text not null
);
INSERT INTO detent_work_items (project_id,id,identifier,title,description,state,author_id,created_at,updated_at,metadata_json,github_issue_number,number) VALUES ('default','z-first','example-org/example-project#10','Example z-first','Prerequisite body','todo','source-author','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z','{"github_issue_number":"10"}',10,1010);
INSERT INTO detent_work_items (project_id,id,identifier,title,description,state,author_id,created_at,updated_at,metadata_json,github_issue_number,number) VALUES ('default','a-second','example-org/example-project#20','Example a-second','Dependent body

Depends on: #10','Retired lane','source-author','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z','{"github_issue_number":"20"}',20,1020);
INSERT INTO detent_work_items (project_id,id,identifier,title,description,state,author_id,created_at,updated_at,metadata_json,github_issue_number,number) VALUES ('default','unused','example-org/example-project#30','Example unused','Closed history','Done','source-author','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z','{"github_issue_number":"30"}',30,1030);
INSERT INTO detent_work_items (project_id,id,identifier,title,state) VALUES ('other','z-first','#10','Other project','Todo');
INSERT INTO detent_work_item_events (project_id,item_id,event_kind,body,payload_json,created_at) VALUES ('default','a-second','comment','Original comment','{}','2026-01-01T00:00:00Z');
INSERT INTO detent_work_item_events (project_id,item_id,event_kind,body,payload_json,created_at) VALUES ('default','a-second','comment_edit','Edited comment','{"comment_id":"1"}','2026-01-02T00:00:00Z');
INSERT INTO detent_work_item_events (project_id,item_id,event_kind,body,payload_json,created_at) VALUES ('default','a-second','comment','Deleted comment','{}','2026-01-01T00:00:00Z');
INSERT INTO detent_work_item_events (project_id,item_id,event_kind,body,payload_json,created_at) VALUES ('default','a-second','comment_delete','','{"comment_id":"3"}','2026-01-02T00:00:00Z');

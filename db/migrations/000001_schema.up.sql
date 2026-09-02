-- Initial schema for the k8Shell Provisioner service.

CREATE SCHEMA IF NOT EXISTS provisioner;

-- org_blueprints stores blueprint definitions scoped to a single
-- organization, alongside (and layered on top of) the file-based, global
-- blueprints the provisioner loads from disk (see internal/blueprint). A
-- blueprint of the same name may exist independently per org, and takes
-- precedence over a file-based blueprint of that name when a user from that
-- org requests it.
--
-- yaml holds the raw blueprint YAML content, in the same shape the
-- ValidateBlueprint/GetBlueprint RPCs accept/return. An org blueprint may
-- reference a file-based Template via its own `template:` field, resolved
-- at load time the same way file-based inheritance is, but it cannot
-- inherit from another org blueprint.
--
-- org references identity.organizations(name): the identity and provisioner
-- services share one physical database (separate schemas, separate
-- migration tables — see MigrationsRoot's per-service
-- x-migrations-table), so this FK requires identity's own schema migration
-- to have already run. No ON DELETE action is set (the default, restrict):
-- identity.DeleteOrganization deletes an org in a transaction and maps any
-- 23503 foreign_key_violation to ErrOrganizationInUse, so a lingering org
-- blueprint here blocks org deletion the same way a lingering user does,
-- rather than being silently orphaned or cascaded away.
CREATE TABLE provisioner.org_blueprints (
    org          varchar     not null references identity.organizations(name),
    name         varchar     not null,
    description  text,
    yaml         bytea       not null,
    is_template  boolean     not null default false,
    created_at   TIMESTAMPTZ not null default now(),
    updated_at   TIMESTAMPTZ not null default now(),

    PRIMARY KEY (org, name)
);

-- Speeds up ListOrgBlueprints, the per-org listing used by both the
-- ListOrgBlueprints RPC and the blueprint manager's full reload.
CREATE INDEX idx_org_blueprints_org ON provisioner.org_blueprints (org);

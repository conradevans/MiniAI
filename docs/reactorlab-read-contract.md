# ReactorLab read contract

MiniAI Phase 1C consumes ReactorLab through the private, versioned
`/internal/miniai/v1` namespace configured by `REACTORLAB_URL`. MiniAI owns
its client DTOs and validation and does not import ReactorLab as a Go
dependency.

## Boundary

The client performs fixed-family GET requests only. Capability arguments can
select a validated application, database, service, result limit, or historical
window; they cannot supply a base URL, arbitrary path, HTTP method, shell
command, SQL, or file name. Reads use short deadlines, bounded bodies, exact
JSON EOF checks, typed field validation, and independent result bounds.

Phase 1C adds no action capabilities, persistence, cache, background poller, or
mutation route.

## Deployment identity

When `source.commitSha` exists, it is the authoritative exact deployed Git
identity. Provider, canonical repository, branch, requested ref,
`activatedAt`, immutable `imageId`, and service image IDs remain typed
facts. Legacy deployments and history may omit these fields; MiniAI preserves
that unknown state and never infers provenance.

`archivedAt` is when a prior version entered rollback history during a later
cutover or rollback. It is distinct from the original `activatedAt`.

Local repository evidence under `/srv` is returned separately. Its branch or
commit may differ from deployed source identity and must not be relabeled as
the deployed version.

## Historical windows and availability

Historical tools pass either `range=15m|1h|6h|24h|7d` or paired RFC3339
`from` and `to` values. ReactorLab remains authoritative for retention and
window semantics. MiniAI validates syntax, pairing, ordering, spans, array
bounds, and fixed endpoint identifiers before consuming responses.

The platform overview preserves each ReactorLab section as available data or
an unavailable stable error code. MiniAI does not fabricate zero-valued
measurements for unavailable sources.

## Intentional compatibility exceptions

Runtime and deployment/build logs continue to use MiniDeploy directly because
ReactorLab Phase 1B intentionally deferred a safe log proxy. Existing
non-agent diagnostic and HTTP compatibility paths may retain legacy reads.
Agent deployment/history/current-state capabilities use the typed ReactorLab
contract. Repository tools remain bounded local reads with their existing path
and secret protections.

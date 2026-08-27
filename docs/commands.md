# The 21 write commands

A field reference for the command plane, derived from Jiku's own contract with
`tools/gendocs`. Regenerate rather than hand-edit — see that command's doc comment for how.

**Why this exists as a separate document.** The read plane needs none: `meta.describe` returns
its whole contract as data, and this client fetches it (`jiku describe`), so it cannot go stale.
There is no equivalent for writes — the command payloads are not discoverable at runtime, so
this is the one thing an integrator cannot get from the server itself.

**Read it as a field list, not as a promise.** Core validates, core decides, and core's rules
move: as write rules migrated out of the api under REQ-007, required fields became optional
(`creator`/`editor`/`author` and `personId` now default from the caller's identity when
absent) and new business-rule error codes appeared. When this document and the server disagree,
the server is right. Report the drift.

## The subject

```
{instance}.{your sub}.jiku-commands.v1.{method}
```

An id goes **in the method**, not in the payload: `requirements.12.edit`. The client builds the
subject — you name a method.

```bash
jiku cmd requirements.12.edit '{"editor":"...","title":"..."}'
```

```go
client.Command(ctx, "requirements.12.edit", payload)
```

## Rules that apply to every command

**Partial edits are three-state.** A field left out is untouched, a field with a value is
replaced, and `null` clears it — except on a field that is mandatory at creation, where `null`
fails. An edit replies `success` with no `data`.

**The acting identity may travel two ways, and only one of them is safe to send yourself.**
Domain fields naming a person — `creator`, `author`, `editor`, `uploader`, `personId`,
`userId` — are ordinary arguments and are sent as-is; several are now optional, because core
resolves them from the caller when absent. The reserved top-level `actor` envelope is different:
**only Jiku's own trusted publisher may send it**, and core answers `invalid_fields` to anyone
else. This client refuses it locally before publishing.

**Who may run which command is deployment policy, not a property of this contract.** A role may
reach a command directly over the bus, only as a side effect of another service acting on its
behalf, or not at all — and that can differ per command within one role. `jiku whoami` reports
what your identity usually reaches; `jiku doctor` reports what it actually does.

**There is no retry and no queue.** Request/reply over core NATS, no JetStream: if core is down
the request times out and the operation did not happen.

## The commands

### `attachments.{id}.delete`

Unlink a file from an entity.

No payload. Send `{}`.

Error codes: `access_denied`, `internal_error`, `invalid_fields`

### `clients.new`

Create an actor.

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | `string` | **yes** |  |
| `description` | `string` | no |  |

Error codes: `access_denied`, `internal_error`, `invalid_fields`

### `clients.{id}.edit`

Edit an actor.

| Field | Type | Required | Notes |
|---|---|---|---|
| `description` | `string` | no | nullable |
| `name` | `string` | no |  |

Error codes: `access_denied`, `client_not_found`, `internal_error`, `invalid_fields`

### `files.request-upload`

Request permission to upload a file.

| Field | Type | Required | Notes |
|---|---|---|---|
| `fileName` | `string` | **yes** |  |
| `fileSize` | `integer` | **yes** |  |
| `mimeType` | `string` | **yes** |  |
| `uploader` | `string` | **yes** |  |
| `checksum` | `string` | no | nullable |

Error codes: `access_denied`, `file_too_large`, `file_type_not_allowed`, `internal_error`, `invalid_fields`

### `files.{fileId}.request-download`

Request permission to download a file.

| Field | Type | Required | Notes |
|---|---|---|---|
| `disposition` | `string` | no | one of: `inline`, `attachment`; default `inline` |

Error codes: `access_denied`, `file_not_found`, `internal_error`

### `projects.new`

Create a project.

| Field | Type | Required | Notes |
|---|---|---|---|
| `code` | `string` | **yes** |  |
| `name` | `string` | **yes** |  |
| `clientId` | `integer` | no | nullable |
| `creator` | `string` | no |  |
| `description` | `string` | no |  |
| `endDate` | `string` | no | format `date-time`; nullable |
| `initDate` | `string` | no | format `date-time` |
| `properties` | `object[]` | no |  |
| `status` | `string` | no | one of: `analisis`, `activo`, `inactivo`, `finalizado`, `cancelado`; default `analisis` |
| `type` | `string` | no | one of: `interno`, `comercial`, `investigacion`, `propuesta`; default `comercial` |

Error codes: `access_denied`, `client_not_found`, `internal_error`, `invalid_fields`

### `projects.{id}.edit`

Edit a project.

| Field | Type | Required | Notes |
|---|---|---|---|
| `clientId` | `integer` | no | nullable |
| `code` | `string` | no |  |
| `description` | `string` | no | nullable |
| `endDate` | `string` | no | format `date-time`; nullable |
| `initDate` | `string` | no | format `date-time` |
| `name` | `string` | no |  |
| `properties` | `object[]` | no |  |
| `status` | `string` | no | one of: `analisis`, `activo`, `inactivo`, `finalizado`, `cancelado` |
| `type` | `string` | no | one of: `interno`, `comercial`, `investigacion`, `propuesta` |

Error codes: `access_denied`, `client_not_found`, `internal_error`, `invalid_fields`, `project_not_found`

### `requirements.new`

Create a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `description` | `string` | **yes** |  |
| `projectId` | `integer` | **yes** |  |
| `title` | `string` | **yes** |  |
| `acceptanceCriteria` | `string` | no | nullable |
| `creator` | `string` | no |  |
| `estimatedFinishDate` | `string` | no | format `date-time`; nullable |
| `fileIds` | `integer[]` | no |  |
| `priority` | `string` | no | one of: `sin_prioridad`, `baja`, `media`, `alta`, `urgente`; default `sin_prioridad` |
| `responsiblePersonIds` | `integer[]` | no |  |
| `scope` | `string` | no | nullable |
| `state` | `string` | no | one of: `analisis`, `planificacion`, `en_cola`, `desarrollo`, `revision`, `resuelto`, `cancelado` |
| `tags` | `object[]` | no |  |
| `technicalSolution` | `string` | no | nullable |
| `type` | `string` | no | one of: `funcionalidad`, `mejora`, `incidencia`, `otro`; nullable |
| `visibilityLevel` | `string` | no | one of: `public`, `internal`; default `public` |

Error codes: `access_denied`, `file_not_owned`, `internal_error`, `invalid_fields`, `invalid_responsible_person`, `project_not_found`

### `requirements.{id}.comment`

Comment on a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `comment` | `string` | **yes** |  |
| `author` | `string` | no |  |
| `fileIds` | `integer[]` | no |  |
| `visibilityLevel` | `string` | no | one of: `public`, `internal`; default `internal` |

Error codes: `access_denied`, `file_not_owned`, `internal_error`, `invalid_fields`, `requirement_not_found`

### `requirements.{id}.edit`

Edit a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `acceptanceCriteria` | `string` | no | nullable |
| `description` | `string` | no |  |
| `editor` | `string` | no |  |
| `estimatedFinishDate` | `string` | no | format `date-time`; nullable |
| `fileIds` | `integer[]` | no |  |
| `priority` | `string` | no | one of: `sin_prioridad`, `baja`, `media`, `alta`, `urgente` |
| `resolutionComment` | `string` | no | nullable |
| `resolutionConclusion` | `string` | no | nullable |
| `resolutionType` | `string` | no | one of: `error_interno`, `fuera_de_alcance`, `error_externo`, `discutible`, `otro`; nullable |
| `responsiblePersonIds` | `integer[]` | no |  |
| `scope` | `string` | no | nullable |
| `state` | `string` | no | one of: `analisis`, `planificacion`, `en_cola`, `desarrollo`, `revision`, `resuelto`, `cancelado` |
| `tags` | `object[]` | no |  |
| `technicalSolution` | `string` | no | nullable |
| `title` | `string` | no |  |
| `type` | `string` | no | one of: `funcionalidad`, `mejora`, `incidencia`, `otro`; nullable |
| `visibilityLevel` | `string` | no | one of: `public`, `internal` |

Error codes: `access_denied`, `file_not_owned`, `internal_error`, `invalid_fields`, `invalid_responsible_person`, `invalid_state_transition`, `requirement_not_found`, `resolution_required`

### `requirements.{id}.resolve`

Resolve a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `type` | `string` | **yes** | one of: `error_interno`, `fuera_de_alcance`, `error_externo`, `discutible`, `otro` |
| `comment` | `string` | no | nullable |
| `conclusion` | `string` | no | nullable |
| `editor` | `string` | no |  |

Error codes: `access_denied`, `internal_error`, `invalid_fields`, `invalid_state_transition`, `requirement_not_found`, `resolution_required`

### `requirements.{id}.subscriptors.new`

Subscribe a user to a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `userId` | `string` | **yes** |  |

Error codes: `access_denied`, `already_subscribed`, `internal_error`, `invalid_fields`, `requirement_not_found`, `user_not_found`

### `requirements.{id}.subscriptors.{userId}.delete`

Unsubscribe a user from a requirement.

No payload. Send `{}`.

Error codes: `access_denied`, `internal_error`, `invalid_date_range`, `invalid_fields`, `subscription_not_found`, `unworked_time_not_found`, `worked_time_not_found`

### `tasks.new`

Create a task.

| Field | Type | Required | Notes |
|---|---|---|---|
| `projectId` | `integer` | **yes** |  |
| `responsiblePersonIds` | `integer[]` | **yes** |  |
| `title` | `string` | **yes** |  |
| `area` | `string` | no | one of: `diseño`, `desarrollo`, `gestion`, `investigacion`; default `desarrollo` |
| `creator` | `string` | no |  |
| `description` | `string` | no | nullable |
| `estimatedFinishDate` | `string` | no | nullable |
| `fileIds` | `integer[]` | no |  |
| `priority` | `string` | no | one of: `sin_prioridad`, `baja`, `media`, `alta`, `urgente`; default `sin_prioridad` |
| `priorityValue` | `integer` | no |  |
| `requirementId` | `integer` | no | nullable |
| `state` | `string` | no | one of: `backlog`, `activo`, `finalizado`, `cancelado`, `en_revision`; default `backlog` |
| `visibilityLevel` | `string` | no | one of: `public`, `internal`; default `public` |

Error codes: `access_denied`, `file_not_owned`, `internal_error`, `invalid_fields`, `person_not_found`, `project_not_found`, `requirement_project_mismatch`

### `tasks.{id}.comment`

Comment on a task.

| Field | Type | Required | Notes |
|---|---|---|---|
| `comment` | `string` | **yes** |  |
| `author` | `string` | no |  |
| `fileIds` | `integer[]` | no |  |
| `visibilityLevel` | `string` | no | one of: `public`, `internal`; default `internal` |

Error codes: `access_denied`, `file_not_owned`, `internal_error`, `invalid_fields`, `objective_not_found`

### `tasks.{id}.edit`

Edit a task.

| Field | Type | Required | Notes |
|---|---|---|---|
| `area` | `string` | no | one of: `diseño`, `desarrollo`, `gestion`, `investigacion` |
| `description` | `string` | no | nullable |
| `editor` | `string` | no |  |
| `estimatedFinishDate` | `string` | no | nullable |
| `fileIds` | `integer[]` | no |  |
| `priority` | `string` | no | one of: `sin_prioridad`, `baja`, `media`, `alta`, `urgente` |
| `priorityValue` | `integer` | no |  |
| `requirementId` | `integer` | no | nullable |
| `responsiblePersonIds` | `integer[]` | no |  |
| `state` | `string` | no | one of: `backlog`, `activo`, `finalizado`, `cancelado`, `en_revision` |
| `title` | `string` | no |  |
| `visibilityLevel` | `string` | no | one of: `public`, `internal` |

Error codes: `access_denied`, `file_not_owned`, `internal_error`, `invalid_fields`, `objective_not_found`, `person_not_found`, `requirement_project_mismatch`

### `unworked-times.new`

Record unworked time.

| Field | Type | Required | Notes |
|---|---|---|---|
| `date` | `string` | **yes** |  |
| `minutes` | `integer` | **yes** |  |
| `personId` | `integer` | **yes** |  |
| `reason` | `string` | **yes** | one of: `tramite`, `corte_servicios`, `vacaciones`, `dia_no_laborable`, `personal`, `medico`, `estudio`, `enfermedad`, `otro` |

Error codes: `access_denied`, `daily_limit_exceeded`, `internal_error`, `invalid_fields`, `person_not_found`

### `unworked-times.{id}.delete`

Delete unworked time.

No payload. Send `{}`.

Error codes: `access_denied`, `internal_error`, `invalid_date_range`, `invalid_fields`, `subscription_not_found`, `unworked_time_not_found`, `worked_time_not_found`

### `week-assigned-times.replace`

Replace a full week of assignments.

| Field | Type | Required | Notes |
|---|---|---|---|
| `assignments` | `object[]` | **yes** |  |
| `dateFrom` | `string` | **yes** |  |

Error codes: `access_denied`, `internal_error`, `invalid_date_range`, `invalid_fields`, `person_not_found`, `project_not_found`

### `worked-times.new`

Log worked time.

| Field | Type | Required | Notes |
|---|---|---|---|
| `date` | `string` | **yes** |  |
| `minutes` | `integer` | **yes** |  |
| `projectId` | `integer` | **yes** |  |
| `personId` | `integer` | no |  |
| `requirementId` | `integer` | no | nullable |
| `taskId` | `integer` | no | nullable |

Error codes: `access_denied`, `daily_limit_exceeded`, `internal_error`, `invalid_date_range`, `invalid_fields`, `objective_not_found`, `person_not_found`, `project_not_found`, `requirement_not_found`, `requirement_project_mismatch`

### `worked-times.{id}.delete`

Delete worked time.

No payload. Send `{}`.

Error codes: `access_denied`, `internal_error`, `invalid_date_range`, `invalid_fields`, `subscription_not_found`, `unworked_time_not_found`, `worked_time_not_found`

## Replies

The same envelope as every other endpoint. A creation returns the new id under `data`; an edit
or delete replies `success` with no `data`.

```json
{ "status": "success", "data": { "id": 8 } }
```

See [protocol.md](protocol.md#the-envelope) for the envelope and the shared error catalog, and
[the compatibility policy](../README.md#compatibility) for why the code catalog is not closed.

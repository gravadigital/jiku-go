# The 20 write commands

A field reference for the command plane, derived from the server's contract.

**Why this exists as a separate document.** The read plane needs none: `meta.describe` returns
its whole contract as data, and this client fetches it (`jiku describe`), so it cannot go stale.
There is no equivalent for writes — the command payloads are not discoverable at runtime, so
this is the one thing an integrator cannot get from the server itself.

**Read it as a field list, not as a promise.** Core validates, core decides, and core's rules
move: as write rules migrate out of the api, required fields can become optional (`personId` is
slated to default from the actor) and new business-rule error codes appear. When this document
and the server disagree, the server is right. Report the drift.

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

**The acting person travels in the payload** — `creator`, `author`, `editor`, `uploader` — because
the subject identifies the *service* that published, and one service user can publish for many
people. Fields naming a person (`personId`, `userId`) are domain arguments and are sent as-is.

**Do not send `actor`.** It is the reserved identity envelope, and only the api's own service
user may carry it; core answers `invalid_fields` to anyone else. This client refuses it locally.

**There is no retry and no queue.** Request/reply over core NATS, no JetStream: if core is down
the request times out and the operation did not happen.

## The commands

### `clients.new`

Create an actor.

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | `string` | **yes** |  |
| `description` | `string` | no |  |

Error codes: `invalid_fields`, `internal_error`

### `clients.{id}.edit`

Edit an actor.

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | `string` | no |  |
| `description` | `string | null` | no | nullable |

Error codes: `invalid_fields`, `client_not_found`, `internal_error`

### `projects.new`

Create a project.

| Field | Type | Required | Notes |
|---|---|---|---|
| `creator` | `string` | **yes** |  |
| `name` | `string` | **yes** |  |
| `code` | `string` | **yes** |  |
| `status` | `string` | no | one of: `analisis`, `activo`, `inactivo`, `finalizado`, `cancelado`; default `analisis` |
| `type` | `string` | no | one of: `interno`, `comercial`, `investigacion`, `propuesta`; default `comercial` |
| `description` | `string` | no |  |
| `initDate` | `string` | no | format `date-time` |
| `endDate` | `string | null` | no | nullable; format `date-time` |
| `clientId` | `integer | null` | no | nullable |
| `properties` | `Property[]` | no |  |

Error codes: `invalid_fields`, `client_not_found`, `internal_error`

### `projects.{id}.edit`

Edit a project.

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | `string` | no |  |
| `code` | `string` | no |  |
| `status` | `string` | no | one of: `analisis`, `activo`, `inactivo`, `finalizado`, `cancelado` |
| `type` | `string` | no | one of: `interno`, `comercial`, `investigacion`, `propuesta` |
| `description` | `string | null` | no | nullable |
| `initDate` | `string` | no | format `date-time` |
| `endDate` | `string | null` | no | nullable; format `date-time` |
| `clientId` | `integer | null` | no | nullable |
| `properties` | `Property[]` | no |  |

Error codes: `invalid_fields`, `project_not_found`, `client_not_found`, `internal_error`

### `tasks.new`

Create a task.

| Field | Type | Required | Notes |
|---|---|---|---|
| `creator` | `string` | **yes** |  |
| `title` | `string` | **yes** |  |
| `description` | `string | null` | no | nullable |
| `estimatedFinishDate` | `string | null` | no | nullable |
| `state` | `string` | no | one of: `backlog`, `activo`, `finalizado`, `cancelado`, `en_revision`; default `backlog` |
| `area` | `string` | no | one of: `diseño`, `desarrollo`, `gestion`, `investigacion`; default `desarrollo` |
| `priority` | `` | no | default `sin_prioridad` |
| `priorityValue` | `integer` | no |  |
| `projectId` | `integer` | **yes** |  |
| `responsiblePersonIds` | `integer[]` | **yes** |  |
| `visibilityLevel` | `string` | no | one of: `public`, `internal`; default `public` |
| `requirementId` | `integer | null` | no | nullable |
| `fileIds` | `` | no |  |

Error codes: `invalid_fields`, `project_not_found`, `person_not_found`, `requirement_project_mismatch`, `file_not_owned`, `internal_error`

### `tasks.{id}.edit`

Edit a task.

| Field | Type | Required | Notes |
|---|---|---|---|
| `editor` | `` | **yes** |  |
| `title` | `string` | no |  |
| `description` | `string | null` | no | nullable |
| `estimatedFinishDate` | `string | null` | no | nullable |
| `state` | `string` | no | one of: `backlog`, `activo`, `finalizado`, `cancelado`, `en_revision` |
| `area` | `string` | no | one of: `diseño`, `desarrollo`, `gestion`, `investigacion` |
| `priority` | `string` | no | one of: `sin_prioridad`, `baja`, `media`, `alta`, `urgente` |
| `priorityValue` | `integer` | no |  |
| `responsiblePersonIds` | `` | no |  |
| `visibilityLevel` | `string` | no | one of: `public`, `internal` |
| `requirementId` | `integer | null` | no | nullable |
| `fileIds` | `` | no |  |

Error codes: `invalid_fields`, `objective_not_found`, `person_not_found`, `requirement_project_mismatch`, `file_not_owned`, `internal_error`

### `tasks.{id}.comment`

Comment on a task.

| Field | Type | Required | Notes |
|---|---|---|---|
| `author` | `string` | **yes** |  |
| `comment` | `string` | **yes** |  |
| `visibilityLevel` | `string` | no | one of: `public`, `internal`; default `internal` |
| `fileIds` | `` | no |  |

Error codes: `invalid_fields`, `objective_not_found`, `file_not_owned`, `internal_error`

### `requirements.new`

Create a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `creator` | `string` | **yes** |  |
| `title` | `string` | **yes** |  |
| `description` | `string` | **yes** |  |
| `projectId` | `integer` | **yes** |  |
| `type` | `string | null` | no | one of: `funcionalidad`, `mejora`, `incidencia`, `otro`, `None`; nullable |
| `priority` | `string` | no | one of: `sin_prioridad`, `baja`, `media`, `alta`, `urgente`; default `sin_prioridad` |
| `visibilityLevel` | `string` | no | one of: `public`, `internal`; default `public` |
| `state` | `string` | no | one of: `analisis`, `planificacion`, `en_cola`, `desarrollo`, `revision`, `resuelto`, `cancelado` |
| `responsiblePersonIds` | `integer[]` | no |  |
| `estimatedFinishDate` | `string | null` | no | nullable; format `date-time` |
| `tags` | `Tag[]` | no |  |
| `fileIds` | `` | no |  |
| `scope` | `string | null` | no | nullable |
| `technicalSolution` | `string | null` | no | nullable |
| `acceptanceCriteria` | `string | null` | no | nullable |

Error codes: `invalid_fields`, `project_not_found`, `invalid_responsible_person`, `file_not_owned`, `internal_error`

### `requirements.{id}.edit`

Edit a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `editor` | `` | **yes** |  |
| `title` | `string` | no |  |
| `description` | `string` | no |  |
| `type` | `string | null` | no | one of: `funcionalidad`, `mejora`, `incidencia`, `otro`, `None`; nullable |
| `priority` | `string` | no | one of: `sin_prioridad`, `baja`, `media`, `alta`, `urgente` |
| `visibilityLevel` | `string` | no | one of: `public`, `internal` |
| `state` | `string` | no | one of: `analisis`, `planificacion`, `en_cola`, `desarrollo`, `revision`, `resuelto`, `cancelado` |
| `responsiblePersonIds` | `` | no |  |
| `estimatedFinishDate` | `string | null` | no | nullable; format `date-time` |
| `tags` | `Tag[]` | no |  |
| `fileIds` | `` | no |  |
| `resolutionType` | `string | null` | no | one of: `error_interno`, `fuera_de_alcance`, `error_externo`, `discutible`, `otro`, `None`; nullable |
| `resolutionConclusion` | `string | null` | no | nullable |
| `resolutionComment` | `string | null` | no | nullable |
| `scope` | `string | null` | no | nullable |
| `technicalSolution` | `string | null` | no | nullable |
| `acceptanceCriteria` | `string | null` | no | nullable |

Error codes: `invalid_fields`, `requirement_not_found`, `invalid_responsible_person`, `file_not_owned`, `internal_error`

### `requirements.{id}.resolve`

Resolve a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `editor` | `string` | **yes** |  |
| `type` | `string` | **yes** | one of: `error_interno`, `fuera_de_alcance`, `error_externo`, `discutible`, `otro` |
| `conclusion` | `string | null` | no | nullable |
| `comment` | `string | null` | no | nullable |

Error codes: `invalid_fields`, `requirement_not_found`, `resolution_required`, `internal_error`

### `requirements.{id}.comment`

Comment on a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `author` | `string` | **yes** |  |
| `comment` | `string` | **yes** |  |
| `visibilityLevel` | `string` | no | one of: `public`, `internal`; default `internal` |
| `fileIds` | `` | no |  |

Error codes: `invalid_fields`, `requirement_not_found`, `file_not_owned`, `internal_error`

### `requirements.{id}.subscriptors.new`

Subscribe a user to a requirement.

| Field | Type | Required | Notes |
|---|---|---|---|
| `userId` | `string` | **yes** |  |

Error codes: `invalid_fields`, `requirement_not_found`, `user_not_found`, `already_subscribed`, `internal_error`

### `requirements.{id}.subscriptors.{userId}.delete`

Unsubscribe a user from a requirement.

No payload. Send `{}`.

Error codes: `invalid_fields`, `subscription_not_found`, `worked_time_not_found`, `unworked_time_not_found`, `internal_error`

### `files.request-upload`

Request permission to upload a file.

| Field | Type | Required | Notes |
|---|---|---|---|
| `uploader` | `` | **yes** |  |
| `fileName` | `string` | **yes** |  |
| `mimeType` | `string` | **yes** |  |
| `fileSize` | `integer` | **yes** |  |
| `checksum` | `string | null` | no | nullable |

Error codes: `invalid_fields`, `file_type_not_allowed`, `file_too_large`, `internal_error`

### `files.{fileId}.request-download`

Request permission to download a file.

| Field | Type | Required | Notes |
|---|---|---|---|
| `disposition` | `string` | no | one of: `inline`, `attachment`; default `inline` |

Error codes: `file_not_found`, `internal_error`

### `attachments.{id}.delete`

Unlink a file from an entity.

No payload. Send `{}`.

Error codes: `invalid_fields`, `internal_error`

### `worked-times.new`

Log worked time.

| Field | Type | Required | Notes |
|---|---|---|---|
| `date` | `string` | **yes** |  |
| `minutes` | `integer` | **yes** |  |
| `projectId` | `integer` | **yes** |  |
| `taskId` | `integer | null` | no | nullable |
| `requirementId` | `integer | null` | no | nullable |
| `personId` | `integer` | **yes** |  |

Error codes: `invalid_fields`, `person_not_found`, `project_not_found`, `objective_not_found`, `requirement_not_found`, `requirement_project_mismatch`, `daily_limit_exceeded`, `internal_error`

### `worked-times.{id}.delete`

Delete worked time.

No payload. Send `{}`.

Error codes: `invalid_fields`, `subscription_not_found`, `worked_time_not_found`, `unworked_time_not_found`, `internal_error`

### `unworked-times.new`

Record unworked time.

| Field | Type | Required | Notes |
|---|---|---|---|
| `date` | `string` | **yes** |  |
| `minutes` | `integer` | **yes** |  |
| `reason` | `string` | **yes** | one of: `tramite`, `corte_servicios`, `vacaciones`, `dia_no_laborable`, `personal`, `medico`, `estudio`, `enfermedad`, `otro` |
| `personId` | `integer` | **yes** |  |

Error codes: `invalid_fields`, `person_not_found`, `daily_limit_exceeded`, `internal_error`

### `unworked-times.{id}.delete`

Delete unworked time.

No payload. Send `{}`.

Error codes: `invalid_fields`, `subscription_not_found`, `worked_time_not_found`, `unworked_time_not_found`, `internal_error`

## Replies

The same envelope as every other endpoint. A creation returns the new id under `data`; an edit
or delete replies `success` with no `data`.

```json
{ "status": "success", "data": { "id": 8 } }
```

See [protocol.md](protocol.md#the-envelope) for the envelope and the shared error catalog, and
[the compatibility policy](../README.md#compatibility) for why the code catalog is not closed.

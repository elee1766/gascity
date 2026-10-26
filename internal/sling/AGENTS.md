# Sling ownership

- A rejected reassignment must preserve the source owner, status, route and linked workflow.
- Standalone built-in reassignment combines route, owner and status in one original-revision conditional write, without retry.
- Refuse formula-backed, convoy-linked, session-linked, held or unsupported-router reassignment before publishing work. Cross-record handoff requires a separate coordinated protocol.
- Routing metadata does not prove that a source/workflow ownership transfer completed.
- Test ownership transitions with isolated real SQLite stores, including separate work and graph stores.

# Sling ownership

- A rejected workflow attachment must preserve the source bead's owner and status.
- Routing metadata does not prove that a source/workflow ownership transfer completed.
- Never clear a newer claim or hold to complete an older reassignment attempt.
- Test ownership transitions with isolated real SQLite stores, including separate work and graph stores.

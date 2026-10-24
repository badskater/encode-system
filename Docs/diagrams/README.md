# Diagrams

Index of diagrams for `encode-system`.

| Diagram | Purpose | Source | Related |
| --- | --- | --- | --- |
| System overview | Controller ↔ nodes ↔ NFS/SMB/S3 topology | `Docs/md/Architecture.md` (ASCII, "System overview") | Architecture §System overview |

The system-overview diagram in `Docs/md/Architecture.md` now depicts a
mixed Windows + Linux fleet with per-node platform heartbeat and
dialect-at-dispatch selection (the controller renders `.ps1` for Windows
nodes, `.sh` for Linux nodes). The ASCII text was updated in this docs
sync; if a rendered image variant of this diagram is ever committed under
`Docs/diagrams/`, it will need a separate refresh to match the mixed-topology
text — no image binaries are tracked today.

Add new diagrams here (Mermaid or image files) and keep this index current.

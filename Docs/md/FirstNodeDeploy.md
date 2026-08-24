# First Node Deployment — Step-by-Step

One-time runbook to bring up the controller and the first Windows Server
2025 encode node. All paths below are relative to the repo root unless noted.

## 0. What you need

| Item | Where |
| --- | --- |
| Controller host | Linux VM/LXC on your PVE cluster, Docker + compose installed, NFS client capable |
| NFS exports | unRAID serving `scripts/` and `ReleaseFolders/` (NFSv3 default) |
| Windows node | Server 2025, reachable from the Ansible control node via WinRM or SSH |
| Encode tools | `DGIndexNV.exe`, `x265_x64.exe` (your fork), `mkvmerge.exe`, `ffmpeg.exe`, `eac3to.exe`, `opusenc.exe`, `SCXvid.exe` |

## 1. Controller (15 min)

```bash
cd docker
cp .env.example .env
```

Edit `.env`:

```bash
ENCODE_ADMIN_USER=admin
ENCODE_ADMIN_PASSWORD=<choose a strong password — used only on first boot>
NFS_SERVER=<ip-or-hostname of the unRAID exporting the shares>
ENCODE_PORT=8080
```

Start it:

```bash
docker compose up -d --build
docker compose logs -f controller   # wait for "controller starting"
```

Verify:

```bash
curl http://localhost:8080/api/health          # -> {"status":"ok"}
```

Open `http://<controller-ip>:8080` and sign in with the admin account
(username `admin` by default, password from ENCODE_ADMIN_PASSWORD or the
one-time generated password in the startup logs). Sessions last 24h with
sliding expiry; wrong passwords lock the login endpoint for 30s after 5
failures. The dashboard loads; the scanner registers series as folders
appear on the share.

Layout inside the container (for reference):

- `/data` — persistent state volume: `encode.db` (SQLite), agent/lib update payloads
- `/data/scripts`, `/data/release` — NFS sub-mounts (same files the Windows nodes see)
- `/app/ui` — baked-in SPA (served via `ENCODE_UI_DIR=/app/ui`)

## 2. Stage the toolchain for Ansible (10 min)

The plays copy binaries from a local staging dir (gitignored):

```bash
cd infra/ansible
mkdir -p files/bin files/agent
# Copy the seven encode tools from wherever you keep them:
cp DGIndexNV.exe x265_x64.exe mkvmerge.exe ffmpeg.exe eac3to.exe opusenc.exe SCXvid.exe files/bin/
# Agent binary + lib (already built in backend/bin):
cp ../../backend/bin/encode-agent.exe ../../powershell/EncodeLib.ps1 files/agent/
```

## 3. Configure the inventory (10 min)

```bash
cp inventory.example inventory.yml
mkdir -p group_vars/all
cp group_vars/secrets.yml.example group_vars/all/secrets.yml
```

> Secrets live in `group_vars/all/` (a directory) because Ansible only
> auto-loads group_vars files matching host/group names — `secrets.yml`
> directly under `group_vars/` would be ignored.

`inventory.yml` — one entry per Windows node, using the hostname the node
should register as:

```yaml
all:
  children:
    encode_nodes:
      hosts:
        enc-01:                      # <- becomes the node name in the UI
          ansible_host: 172.24.9.61
          # WinRM over HTTPS (recommended):
          ansible_connection: winrm
          ansible_winrm_transport: ntlm
          ansible_winrm_server_cert_validation: ignore
```

`group_vars/all/secrets.yml`:

```yaml
ansible_user: Administrator
ansible_password: "<windows admin password>"

# Credentials for each node — pick ONE form per host (see step 4):
encode_node_tokens: {}
encode_pairing_codes: {}
```

Also check `group_vars/all.yml` — set `encode_controller_url` to the URL the
Windows nodes will use to reach the controller (e.g.
`http://172.24.9.55:8080`), and the NFS server/paths if they differ from the
defaults.

**Reachability check before continuing:**

```bash
ansible -i inventory.yml encode_nodes -m ansible.windows.win_ping
```

If WinRM isn't enabled on the node yet: run `winrm quickconfig` (as
Administrator) on the node, or enable OpenSSH Server and switch the
inventory to `ansible_connection: ssh`.

## 4. Node credentials — pick one

**Option A — pairing code (recommended, zero-touch):**

1. UI → **Nodes** → *Issue pairing code* (valid 1 hour). Copy it.
2. In `group_vars/all/secrets.yml`:

   ```yaml
   encode_pairing_codes:
     enc-01: "<paste the code>"
   ```

The agent registers itself on first start and stores its own credential;
the code is consumed and never needed again.

**Option B — manual token:**

1. UI → **Nodes** → register `enc-01` → copy the shown token (shown once).
2. In `group_vars/all/secrets.yml`:

   ```yaml
   encode_node_tokens:
     enc-01: "<paste the token>"
   ```

## 5. Deploy the node (10 min)

```bash
cd infra/ansible
ansible-playbook -i inventory.yml site.yml
```

The three plays run in order:

1. **nfs-client** — installs the Windows NFS client feature, mounts both
   shares to `C:\Encodes\scripts` and `C:\Encodes\ReleaseFolders`, and
   registers a startup task that re-mounts them after reboot.
2. **bin-tools** — deploys the seven tools into `C:\bin`.
3. **agent** — installs `encode-agent.exe` + `EncodeLib.ps1` under
   `C:\encode-agent`, writes `agent.json`, registers the `encode-agent`
   service (auto-start) and starts it.

## 6. Verify the node

In the UI (**Dashboard**):

- `enc-01` appears within one heartbeat interval (default 5 s), status
  **idle**.

Then run the smoke episode:

```bash
# On the NFS server (or any client with write access):
mkdir -p "<scripts export>/Smoke Test/Ep 01"
# copy a small source file as src.m2ts and any 1080.avs/1080.vpy next to it
```

Watch the UI (**Jobs**): the scanner creates the job (`pending`), the node
claims it (`running`, live step/progress updates), and it finishes `done`.
Check the release share gained `[OldFartsSubs] Smoke Test - Raws [1080p]/`.

## 7. Add more nodes

Repeat steps 3–5 per node. Episodes distribute across all enabled idle nodes
automatically (one job per node at a time). Use the **Series** page to give
individual series their own flow; **Flows** to create more sequences and mark
the default.

> **Cloned-VM hazard.** If you add a node by cloning a VM that already ran an
> agent, the clone inherits node 1's identity (persisted credential +
> scheduled tasks) and would heartbeat as the SAME node. After cloning, wipe
> the inherited state before first start:
>
> ```powershell
> Remove-Item C:\encode-agent\agent.json, C:\encode-agent\node.token -Force -ErrorAction SilentlyContinue
> Remove-Item C:\encode-agent-dist\agent.json, C:\encode-agent-dist\node.token -Force -ErrorAction SilentlyContinue
> schtasks /delete /tn EncodeAgent /f; schtasks /delete /tn EncodeAgentDist /f
> ```
>
> Then pair the clone fresh (issue a pairing code and point `agent.json` at
> it). Verified live: a clone of the test node was cleaned and paired as a
> distinct second worker, and jobs distributed across both.

## Rollback & recovery

- **Controller:** `docker compose down` never touches the state volume; the
  DB survives. Snapshot `encode-state` (`docker run --rm -v
  encode-state:/data -v $(pwd):/backup alpine tar czf /backup/db-backup.tgz
  /data/encode.db`) before major upgrades.
- **Agent:** the update store holds every published version; revert the
  manifest in the DB to downgrade all nodes on their next heartbeat.
- **Ansible:** plays are idempotent — re-run after fixing config; there is
  no destructive teardown path.

## Troubleshooting

| Symptom | First check |
| --- | --- |
| Node never appears | `C:\encode-agent\agent.log` — pairing/401 means credential issue; connection refused means `encode_controller_url` is wrong |
| Job stuck `assigned` | Agent claimed but PowerShell failed — job's log tail in the UI shows the failing step and tool |
| `required tool not found` | Tool missing from `C:\bin` on that node — re-run `site.yml` |
| Scanner doesn't create jobs | Folder needs a source media file **and** a `.avs`/`.vpy`; check controller logs for skip reasons |
| Agent warns "plain HTTP" | Expected on trusted LANs; put a reverse proxy with TLS in front for anything else |

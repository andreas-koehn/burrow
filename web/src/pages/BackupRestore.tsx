import { useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, downloadFile, ApiError } from "@/lib/api";
import { Button, Dialog, EmptyState, PageHeader, SkeletonRows } from "@/components/ds";
import { formatBytes } from "@/lib/format";
import type { BackupRow } from "@/lib/contract";

function copy(text: string) {
  void navigator.clipboard?.writeText(text);
}

function truncate(s: string, n = 12): string {
  return s.length <= n ? s : `${s.slice(0, n)}…`;
}

export default function BackupRestore() {
  const qc = useQueryClient();
  const backups = useQuery({
    queryKey: ["backups"],
    queryFn: () => apiFetch<BackupRow[]>("/backups"),
    retry: false,
  });

  const fileRef = useRef<HTMLInputElement>(null);
  const [restoreFile, setRestoreFile] = useState<File | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);

  const create = useMutation({
    mutationFn: () => apiFetch<{ id: string; started_at: string }>("/backups", { method: "POST", body: "{}" }),
    onSuccess: () => {
      toast.success("Backup queued.");
      qc.invalidateQueries({ queryKey: ["backups"] });
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't queue backup."),
  });

  const verify = useMutation({
    mutationFn: (id: string) =>
      apiFetch<{ ok: boolean; sha256_match: boolean }>(`/backups/${id}/verify`, { method: "POST", body: "{}" }),
    onSuccess: (res) => toast.success(res.sha256_match ? "Backup is intact." : "Backup mismatch."),
  });
  const remove = useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/backups/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["backups"] }),
  });
  const restore = useMutation({
    mutationFn: async () => {
      if (!restoreFile) throw new Error("no file");
      const fd = new FormData();
      fd.append("file", restoreFile);
      return apiFetch<{ restore_id: string }>("/backups/restore", {
        method: "POST",
        // FormData: drop the JSON Content-Type so the browser sets the multipart boundary.
        headers: { "Content-Type": "" },
        body: fd as unknown as BodyInit,
      });
    },
    onSuccess: () => {
      toast.success("Restore queued.");
      setConfirmOpen(false);
      setRestoreFile(null);
    },
  });

  return (
    <div className="backup-page">
      <PageHeader
        title="Backup & restore"
        subtitle={<>Backups include the database, the relay&apos;s TLS cert state, and config — but<strong> not</strong> ephemeral session/audit-chain pointers reset on restore.</>}
        actions={<Button variant="primary" size="sm" disabled={create.isPending} onClick={() => create.mutate()}>{create.isPending ? "Creating…" : "Create backup"}</Button>}
      />

      <section className="card">
        <h2 className="card-title">Backup history</h2>
        {!backups.data ? (
          <SkeletonRows n={2} />
        ) : backups.data.length === 0 ? (
          <EmptyState title="No backups yet">
            Create a snapshot to capture the relay's database, TLS state, and config.
          </EmptyState>
        ) : (
          <div className="table-wrap">
            <table className="data" aria-label="Backup history">
              <thead>
                <tr><th>Taken</th><th>Size</th><th>Version</th><th>SHA-256</th><th className="col-actions"></th></tr>
              </thead>
              <tbody>
                {backups.data.map((b) => (
                  <tr key={b.id}>
                    <td className="mono small">{b.taken_at}</td>
                    <td className="mono small">{formatBytes(b.size_bytes)}</td>
                    <td>{b.version}</td>
                    <td>
                      <span className="row row-center gap-2">
                        <code className="mono small">{truncate(b.db_sha256)}</code>
                        <button type="button" className="icon-btn" aria-label={`Copy sha for ${b.id}`} onClick={() => { copy(b.db_sha256); toast.success("Copied."); }}>
                          <Copy size={13} />
                        </button>
                      </span>
                    </td>
                    <td className="col-actions">
                      <Button variant="ghost" size="sm" onClick={() => { void downloadFile(`/backups/${b.id}/download`, `${b.id}.burrow`); }}>Download</Button>
                      {" "}
                      <Button variant="ghost" size="sm" onClick={() => verify.mutate(b.id)}>Verify</Button>
                      {" "}
                      <Button variant="destructive" size="sm" onClick={() => remove.mutate(b.id)}>Delete</Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="card">
        <h2 className="card-title">Restore from backup</h2>
        <p className="muted">Upload a previously-downloaded backup archive to replace the current Burrow state.</p>
        <div className="row row-center gap-2">
          <input
            ref={fileRef}
            id="restore-file"
            type="file"
            accept=".tar.gz,.tgz,application/gzip"
            onChange={(e) => setRestoreFile(e.target.files?.[0] ?? null)}
            style={{
              position: "absolute",
              width: 1,
              height: 1,
              padding: 0,
              margin: -1,
              overflow: "hidden",
              clip: "rect(0 0 0 0)",
              whiteSpace: "nowrap",
              borderWidth: 0,
            }}
          />
          <label htmlFor="restore-file" className="btn btn-secondary btn-sm">
            Choose backup archive…
          </label>
          <span className="mono small muted" aria-live="polite">
            {restoreFile
              ? `${restoreFile.name} · ${formatBytes(restoreFile.size)}`
              : "no file selected"}
          </span>
          <Button
            variant="destructive"
            size="sm"
            disabled={!restoreFile}
            onClick={() => setConfirmOpen(true)}
          >
            Restore
          </Button>
        </div>
      </section>

      <Dialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title="Restore from backup?"
        footer={
          <>
            <Button variant="secondary" onClick={() => setConfirmOpen(false)}>Cancel</Button>
            <Button variant="destructive" disabled={restore.isPending} onClick={() => restore.mutate()}>
              Restore
            </Button>
          </>
        }
      >
        <p>
          Replace current Burrow state with backup {restoreFile?.name ?? "?"}? Active client
          sessions and the audit chain will be reset.
        </p>
      </Dialog>
      <Toaster />
    </div>
  );
}

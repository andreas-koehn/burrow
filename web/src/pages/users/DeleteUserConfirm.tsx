import { AlertTriangle } from "lucide-react";
import { Button, Dialog } from "@/components/ds";

/** The confirmation before a user is deleted; shared by the users list and the edit dialog. Rendered open: mount it while confirming. */
export function DeleteUserConfirm({ email, pending, onConfirm, onCancel }: {
  email: string | undefined;
  pending: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!o) onCancel(); }}
      title="Delete user?"
      description={`${email} will lose access immediately, and their tokens and services are removed. This cannot be undone.`}
      footer={
        <>
          <Button variant="secondary" onClick={onCancel}>Cancel</Button>
          <Button variant="destructive-solid" disabled={pending} onClick={onConfirm}>Delete user</Button>
        </>
      }
    >
      <div className="confirm-icon" aria-hidden="true"><AlertTriangle size={16} /></div>
    </Dialog>
  );
}

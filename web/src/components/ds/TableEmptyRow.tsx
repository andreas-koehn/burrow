import type { ReactNode } from "react";

export interface TableEmptyRowProps {
  colSpan: number;
  title: ReactNode;
  /** Optional one-line hint telling the user how the table gets its first row. */
  children?: ReactNode;
}

/* Empty state for a data table: keeps the header row visible and replaces the
   body with one centred cell, so "nothing here" never looks like a data row. */
export function TableEmptyRow({ colSpan, title, children }: TableEmptyRowProps) {
  return (
    <tr>
      <td colSpan={colSpan} className="table-empty">
        <span className="table-empty-title">{title}</span>
        {children != null && <span className="table-empty-hint">{children}</span>}
      </td>
    </tr>
  );
}

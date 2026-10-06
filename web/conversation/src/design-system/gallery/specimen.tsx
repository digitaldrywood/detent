// Specimen types and the gallery's own layout helpers.
//
// The helpers only arrange and caption specimens: they are gallery chrome, not
// product UI, and they use the app's semantic tokens so a frame reads in its
// own theme. Every control a specimen shows is the real component.
import type React from "react";

import type { CatalogEntry } from "./catalog";

export interface Specimen {
  readonly id: string;
  readonly title: string;
  /** One line under the title: what the specimen demonstrates. */
  readonly note?: string;
  /**
   * The least height the frame keeps for this specimen, so an overlay opened
   * from it has room inside the frame document.
   */
  readonly minHeight?: number;
  /**
   * A fixed frame height, for compositions that fill their viewport
   * (`h-dvh`, virtualized lists). Without it such a specimen would grow the
   * frame it measures itself against.
   */
  readonly height?: number;
  readonly render: () => React.ReactNode;
}

/** The catalog fields the gallery shows when the catalog has no entry yet. */
export type EntryMeta = Pick<CatalogEntry, "name" | "kind" | "group"> &
  Partial<Pick<CatalogEntry, "source">>;

export type GalleryDoc =
  | { readonly meta: EntryMeta; readonly specimens: readonly Specimen[] }
  | { readonly meta: EntryMeta; readonly excluded: string };

/** A captioned cell: the label under a specimen, like a type specimen sheet. */
export function Cell({
  label,
  children,
  className,
}: {
  readonly label: string;
  readonly children: React.ReactNode;
  readonly className?: string;
}): React.ReactElement {
  return (
    <figure className={`m-0 flex min-w-0 flex-col items-start gap-1.5 ${className ?? ""}`}>
      <div className="flex min-h-8 max-w-full items-center">{children}</div>
      <figcaption className="font-mono text-[11px] text-muted-foreground">{label}</figcaption>
    </figure>
  );
}

/** A wrapping row of cells. */
export function Row({
  children,
  className,
}: {
  readonly children: React.ReactNode;
  readonly className?: string;
}): React.ReactElement {
  return <div className={`flex flex-wrap items-start gap-x-5 gap-y-4 ${className ?? ""}`}>{children}</div>;
}

/** A labelled matrix: one row per `rows` key, one cell per `columns` key. */
export function Matrix<R extends string, C extends string>({
  rows,
  columns,
  render,
}: {
  readonly rows: readonly R[];
  readonly columns: readonly C[];
  readonly render: (row: R, column: C) => React.ReactNode;
}): React.ReactElement {
  return (
    <div className="max-w-full overflow-x-auto">
      <table className="border-separate border-spacing-x-4 border-spacing-y-3">
        <thead>
          <tr>
            <th />
            {columns.map((column) => (
              <th
                key={column}
                className="text-left font-mono text-[11px] font-normal text-muted-foreground"
                scope="col"
              >
                {column}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row}>
              <th
                className="pe-2 text-left align-middle font-mono text-[11px] font-normal text-muted-foreground"
                scope="row"
              >
                {row}
              </th>
              {columns.map((column) => (
                <td key={column} className="align-middle">
                  {render(row, column)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** Every key of a variant union, checked against the component's own type. */
export function keysOf<K extends string>(record: Record<K, true>): K[] {
  return Object.keys(record) as K[];
}

export const LONG_LABEL =
  "A deliberately long label that keeps going so truncation and wrapping show up";

/** One entry of a list of what a form still lacks; anything falsy is not missing. */
export type Missing = string | false | null | undefined;

/** The first entry that names something missing, or undefined when nothing is. */
export function firstMissing(missing: Missing[]): string | undefined {
  return missing.find((m): m is string => typeof m === "string" && m !== "");
}

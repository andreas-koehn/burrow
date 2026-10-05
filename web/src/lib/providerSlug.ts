import { slugError } from "@/components/SlugField";

/** Local validation message for a provider slug, or null when it is fine or empty. */
export function providerSlugError(value: string): string | null {
  if (value === "v1") return '"v1" is reserved.';
  return slugError(value);
}

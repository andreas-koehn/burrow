/**
 * Quote one argument for a POSIX shell. Safe args are returned unchanged so
 * the common command stays readable; anything else is single-quoted.
 */
export function shellQuote(arg: string): string {
  if (/^[A-Za-z0-9_\-./:@=+,]+$/.test(arg)) return arg;
  return `'${arg.replace(/'/g, `'\\''`)}'`;
}

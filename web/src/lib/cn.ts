export type ClassValue = string | false | null | undefined

/** Join conditional class names. */
export function cn(...values: ClassValue[]): string {
  return values.filter(Boolean).join(' ')
}

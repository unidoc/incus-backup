/** The word only ("snapshot" / "snapshots"); lib/format's plural() also prints the number. */
export const noun = (n: number, one: string, many = one + 's') => (n === 1 ? one : many)

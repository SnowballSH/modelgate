const usd = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});

export function formatUsd(amount: number): string {
  return usd.format(amount);
}

const dateTime = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
});

export function formatTimestamp(rfc3339: string | null): string {
  if (rfc3339 === null) return "never";
  const parsed = new Date(rfc3339);
  if (Number.isNaN(parsed.getTime())) return rfc3339;
  return dateTime.format(parsed);
}

export function formatMonth(month: string): string {
  const parsed = new Date(`${month}-01T00:00:00`);
  if (Number.isNaN(parsed.getTime())) return month;
  return new Intl.DateTimeFormat(undefined, {
    month: "long",
    year: "numeric",
  }).format(parsed);
}

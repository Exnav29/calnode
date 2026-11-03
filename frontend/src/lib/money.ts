// Display formatting for amounts in minor units. Matches the public pages
// (handler/directory.go listPrice): GHS reads GH₵1,800, and whole amounts drop
// the ".00". Exact amounts (receipts, refunds) keep both decimals.
const symbols: Record<string, string> = {
	USD: '$', EUR: '€', GBP: '£', AUD: 'A$', CAD: 'C$', NZD: 'NZ$', GHS: 'GH₵'
};

export function fmtMoney(cents?: number, currency?: string, opts: { exact?: boolean } = {}): string {
	if (!cents) return '';
	const c = (currency || 'usd').toUpperCase();
	const whole = cents % 100 === 0 && !opts.exact;
	const amt = (cents / 100).toLocaleString('en-US', {
		minimumFractionDigits: whole ? 0 : 2,
		maximumFractionDigits: 2
	});
	return symbols[c] ? symbols[c] + amt : `${amt} ${c}`;
}

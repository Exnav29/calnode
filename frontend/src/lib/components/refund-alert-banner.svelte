<script lang="ts">
	// Unresolved Paystack refund exceptions (needs-attention / failed) and refunded bookings
	// the monitor could not check. Shown on every admin page so an exception stays visible
	// even if the alert email could not be sent. Renders nothing when all is well.
	import { onMount } from 'svelte';
	import { toast } from 'svelte-sonner';
	import { api } from '$lib/api';
	import { Button } from '$lib/components/ui/button';
	import { ConfirmDialog } from '$lib/components/ui/confirm-dialog';

	type RefundException = {
		refund_id: number;
		booking_id: string;
		reference: string;
		amount: string;
		status: string;
		reason: string;
		status_changed_at: string;
		alerted_at?: string;
		alert_error?: string;
	};
	type RefundCheckError = { booking_id: string; reference: string; error: string; checked_at: string };

	let exceptions = $state<RefundException[]>([]);
	let checkErrors = $state<RefundCheckError[]>([]);
	let ackTarget = $state<RefundException | null>(null);
	let ackOpen = $state(false);

	async function load() {
		try {
			const r = await api.get<{ exceptions: RefundException[]; check_errors: RefundCheckError[] }>(
				'/v1/paystack/refund-exceptions'
			);
			exceptions = r.exceptions;
			checkErrors = r.check_errors;
		} catch {
			// Non-critical: the banner just doesn't show; the alert email is independent.
		}
	}

	async function acknowledge() {
		if (!ackTarget) return;
		try {
			await api.post(`/v1/paystack/refund-exceptions/${ackTarget.refund_id}/acknowledge`);
			toast.success(`Refund ${ackTarget.refund_id} acknowledged`);
			await load();
		} catch (e: any) {
			toast.error(e.message ?? 'Could not acknowledge');
		}
	}

	onMount(() => {
		load();
		const t = setInterval(load, 5 * 60 * 1000);
		return () => clearInterval(t);
	});
</script>

<ConfirmDialog
	bind:open={ackOpen}
	title="Acknowledge this refund exception?"
	description="Only do this once the customer has been refunded or the issue is otherwise dealt with. It hides the refund from this banner; monitoring continues, and it alerts again if Paystack reports a new problem."
	confirmText="Acknowledge"
	onConfirm={acknowledge}
/>

{#if exceptions.length || checkErrors.length}
	<div class="space-y-1.5 border-b border-red-200 bg-red-50 px-4 py-2 text-sm text-red-900">
		{#each exceptions as e (e.refund_id)}
			<div class="flex items-center justify-between gap-3">
				<p>
					<span class="font-semibold">
						Paystack refund {e.refund_id} {e.status === 'failed' ? 'failed' : 'needs attention'}
					</span>
					— {e.amount}, booking {e.booking_id}{e.reason ? ` (${e.reason})` : ''}.
					{#if e.status === 'failed'}
						Open it in the Paystack Dashboard under Refunds to see why, then retry or refund another way.
					{:else}
						Get the customer's account (for Mobile Money, the wallet number), then retry it in the Paystack
						Dashboard under Refunds → Needs attention.
					{/if}
					<a
						href="https://dashboard.paystack.com/#/refunds"
						target="_blank"
						rel="noopener noreferrer"
						class="ml-1 font-medium underline">Open Paystack</a
					>
					{#if e.alert_error}
						<span class="ml-1 text-red-700">Alert email not sent yet: {e.alert_error}</span>
					{/if}
				</p>
				{#if e.status === 'failed'}
					<Button
						variant="outline"
						size="sm"
						class="shrink-0 border-red-300 bg-white hover:bg-red-100"
						onclick={() => {
							ackTarget = e;
							ackOpen = true;
						}}
					>
						Acknowledge
					</Button>
				{/if}
			</div>
		{/each}
		{#each checkErrors as c (c.booking_id)}
			<p>
				<span class="font-semibold">Refund status unknown</span> for booking {c.booking_id}: {c.error}.
				Check it in the Paystack Dashboard.
			</p>
		{/each}
	</div>
{/if}

<script lang="ts">
	import { onMount } from 'svelte';
	import { api, type StripeSettings, type PaystackSettings } from '$lib/api';
	import { currentUser } from '$lib/stores';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { ConfirmDialog } from '$lib/components/ui/confirm-dialog';
	import { toast } from 'svelte-sonner';
	import { saveOnCmdS } from '$lib/save-shortcut';
	import { createAsyncFlag } from '$lib/async-action.svelte';

	const loadingFlag = createAsyncFlag(true);
	const savingFlag = createAsyncFlag();
	let confirmDisconnectOpen = $state(false);

	let settings = $state<StripeSettings | null>(null);
	let secretKey = $state('');
	let publishableKey = $state('');
	let webhookSecret = $state('');

	const webhookURL = $derived(settings?.webhook_url || '');

	// Paystack (second provider). The secret key's prefix decides test vs live.
	const psSavingFlag = createAsyncFlag();
	let confirmPsDisconnectOpen = $state(false);
	let ps = $state<PaystackSettings | null>(null);
	let psSecretKey = $state('');
	let psPublicKey = $state('');

	onMount(() => loadingFlag.run(async () => {
		[settings, ps] = await Promise.all([
			api.get<StripeSettings>('/v1/settings/stripe'),
			api.get<PaystackSettings>('/v1/settings/paystack'),
		]);
		publishableKey = settings.publishable_key;
		psPublicKey = ps.public_key;
	}, 'Could not load payment settings'));

	async function savePaystack() {
		await psSavingFlag.run(async () => {
			const body: Record<string, unknown> = { public_key: psPublicKey.trim() };
			if (psSecretKey) body.secret_key = psSecretKey.trim();
			ps = await api.patch<PaystackSettings>('/v1/settings/paystack', body);
			psSecretKey = '';
			toast.success(ps.mode === 'live' ? 'Saved — Paystack is LIVE' : 'Saved — Paystack is in test mode');
		}, 'Could not save Paystack settings');
	}

	async function disconnectPaystack() {
		await psSavingFlag.run(async () => {
			ps = await api.patch<PaystackSettings>('/v1/settings/paystack', { clear: true });
			psSecretKey = psPublicKey = '';
			toast.success('Paystack disconnected');
		}, 'Could not disconnect Paystack');
	}

	async function save() {
		await savingFlag.run(async () => {
			const body: Record<string, unknown> = { publishable_key: publishableKey };
			if (secretKey) body.secret_key = secretKey;
			if (webhookSecret) body.webhook_secret = webhookSecret;
			settings = await api.patch<StripeSettings>('/v1/settings/stripe', body);
			secretKey = '';
			webhookSecret = '';
			toast.success('Saved — set a price on an event type to start charging');
		}, 'Could not save payment settings');
	}

	async function disconnect() {
		await savingFlag.run(async () => {
			settings = await api.patch<StripeSettings>('/v1/settings/stripe', { clear: true });
			secretKey = publishableKey = webhookSecret = '';
			toast.success('Stripe disconnected');
		}, 'Could not disconnect');
	}
</script>

<svelte:window onkeydown={saveOnCmdS(save, () => !savingFlag.active)} />

{#if !$currentUser?.is_admin}
	<p class="text-sm text-muted-foreground">Admin access required.</p>
{:else}

{#if loadingFlag.active}
	<p class="py-8 text-sm text-muted-foreground">Loading…</p>
{:else}
	<div class="max-w-lg space-y-4">

		{#if !settings?.configured}
		<div class="rounded-lg border bg-card p-6">
			<h2 class="mb-4 text-sm font-semibold">Setup instructions</h2>
			<ol class="space-y-4 text-sm">
				<li class="flex gap-3">
					<span class="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold text-muted-foreground">1</span>
					<div>
						In your <a href="https://dashboard.stripe.com/apikeys" target="_blank" rel="noopener noreferrer" class="font-medium text-primary underline">Stripe Dashboard → Developers → API keys</a>,
						copy the <span class="font-medium">Secret key</span> and <span class="font-medium">Publishable key</span>.
					</div>
				</li>
				<li class="flex gap-3">
					<span class="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold text-muted-foreground">2</span>
					<div>
						Go to <span class="font-medium">Developers → Webhooks → Add endpoint</span> and set the URL to:
						<code class="mt-1 block rounded bg-muted px-2 py-1 text-xs font-mono break-all">{webhookURL}</code>
						Subscribe to <code class="rounded bg-muted px-1">checkout.session.completed</code> and
						<code class="rounded bg-muted px-1">checkout.session.expired</code>.
					</div>
				</li>
				<li class="flex gap-3">
					<span class="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold text-muted-foreground">3</span>
					<div>
						Copy the endpoint's <span class="font-medium">Signing secret</span> (<code class="rounded bg-muted px-1">whsec_…</code>),
						paste all three values below, and save.
					</div>
				</li>
			</ol>
		</div>
		{/if}

		<div class="rounded-lg border bg-card p-6">
			<div class="mb-4 flex items-start justify-between gap-2">
				<div>
					<h2 class="text-sm font-semibold">Stripe</h2>
					<p class="mt-0.5 text-xs text-muted-foreground">
						Lets you charge for bookings. An event type with a price sends the booker to Stripe
						Checkout before the slot is confirmed.
					</p>
				</div>
				{#if settings !== null}
					<span class="flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium {settings.configured ? 'bg-green-50 text-green-700' : 'bg-amber-50 text-amber-700'}">
						<span class="h-1.5 w-1.5 rounded-full {settings.configured ? 'bg-green-500' : 'bg-amber-400'}"></span>
						{settings.configured ? 'Configured' : 'Not configured'}
					</span>
				{/if}
			</div>

			<div class="space-y-3">
				<div class="space-y-1.5">
					<Label for="s-secret">Secret key</Label>
					<Input id="s-secret" type="password"
						placeholder={settings?.secret_key_set ? '•••••••• (stored)' : 'sk_live_… or sk_test_…'}
						bind:value={secretKey} />
					{#if settings?.secret_key_set && !secretKey}
						<p class="text-xs text-muted-foreground">Stored — leave blank to keep it.</p>
					{/if}
				</div>
				<div class="space-y-1.5">
					<Label for="s-pub">Publishable key</Label>
					<Input id="s-pub" type="text" placeholder="pk_live_… or pk_test_…" bind:value={publishableKey} />
				</div>
				<div class="space-y-1.5">
					<Label for="s-wh">Webhook signing secret</Label>
					<Input id="s-wh" type="password"
						placeholder={settings?.webhook_secret_set ? '•••••••• (stored)' : 'whsec_…'}
						bind:value={webhookSecret} />
					{#if settings?.webhook_secret_set && !webhookSecret}
						<p class="text-xs text-muted-foreground">Stored — leave blank to keep it.</p>
					{/if}
				</div>
			</div>

			{#if settings?.configured}
				<div class="mt-5 border-t pt-4">
					<p class="text-xs font-medium text-muted-foreground">Webhook endpoint</p>
					<p class="mt-0.5 text-xs text-muted-foreground">Register this in Stripe → Developers → Webhooks.</p>
					<code class="mt-2 block rounded bg-muted px-2 py-1 text-xs font-mono break-all">{webhookURL}</code>
				</div>
			{/if}

			<div class="mt-5 flex items-center gap-2">
				<Button onclick={save} disabled={savingFlag.active}>{savingFlag.active ? 'Saving…' : 'Save'}</Button>
				{#if settings?.configured}
					<Button variant="outline" onclick={() => (confirmDisconnectOpen = true)} disabled={savingFlag.active}>Disconnect</Button>
				{/if}
			</div>
		</div>

		<div class="rounded-lg border bg-card p-6">
			<div class="mb-4 flex items-start justify-between gap-2">
				<div>
					<h2 class="text-sm font-semibold">Paystack</h2>
					<p class="mt-0.5 text-xs text-muted-foreground">
						Charge in GHS (or any currency your Paystack account accepts). Choose Paystack per
						event type and set its exact charge there.
					</p>
				</div>
				{#if ps !== null}
					<span class="flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium {ps.mode === 'live' ? 'bg-green-50 text-green-700' : ps.mode === 'test' ? 'bg-blue-50 text-blue-700' : 'bg-amber-50 text-amber-700'}">
						<span class="h-1.5 w-1.5 rounded-full {ps.mode === 'live' ? 'bg-green-500' : ps.mode === 'test' ? 'bg-blue-500' : 'bg-amber-400'}"></span>
						{ps.mode === 'live' ? 'Live' : ps.mode === 'test' ? 'Test mode' : 'Not configured'}
					</span>
				{/if}
			</div>

			<div class="space-y-3">
				<div class="space-y-1.5">
					<Label for="p-secret">Secret key</Label>
					<Input id="p-secret" type="password"
						placeholder={ps?.secret_key_set ? '•••••••• (stored)' : 'sk_test_… or sk_live_…'}
						bind:value={psSecretKey} />
					{#if ps?.secret_key_set && !psSecretKey}
						<p class="text-xs text-muted-foreground">Stored — leave blank to keep it.</p>
					{/if}
				</div>
				<div class="space-y-1.5">
					<Label for="p-pub">Public key</Label>
					<Input id="p-pub" type="text" placeholder="pk_test_… or pk_live_…" bind:value={psPublicKey} />
				</div>
			</div>

			{#if ps?.mode === 'test'}
				<p class="mt-4 rounded-md bg-blue-50 px-3 py-2 text-xs text-blue-800">
					Test mode: Paystack-priced event types only take bookings from a page opened with
					<code class="rounded bg-white/60 px-1">?paystack=test</code>, so no visitor can reach a test checkout by accident.
				</p>
			{/if}

			{#if ps?.configured}
				<div class="mt-5 space-y-3 border-t pt-4 text-xs text-muted-foreground">
					<div>
						<p class="font-medium">Payments are confirmed without a webhook</p>
						<p class="mt-0.5">Each payment is verified with Paystack's API when the booker returns, and every minute in the background, so the account's existing webhook URL does not need to change.</p>
					</div>
					<div>
						<p class="font-medium">Optional webhook endpoint</p>
						<code class="mt-1 block rounded bg-muted px-2 py-1 font-mono break-all">{ps.webhook_url}</code>
					</div>
				</div>
			{/if}

			<div class="mt-5 flex items-center gap-2">
				<Button onclick={savePaystack} disabled={psSavingFlag.active}>{psSavingFlag.active ? 'Saving…' : 'Save'}</Button>
				{#if ps?.configured}
					<Button variant="outline" onclick={() => (confirmPsDisconnectOpen = true)} disabled={psSavingFlag.active}>Disconnect</Button>
				{/if}
			</div>
		</div>
	</div>
{/if}

<ConfirmDialog
	bind:open={confirmPsDisconnectOpen}
	title="Remove Paystack credentials?"
	description="Paystack-priced event types will stop being bookable. Pending holds are kept until they can be verified again."
	confirmText="Remove"
	destructive
	onConfirm={disconnectPaystack}
/>


<ConfirmDialog
	bind:open={confirmDisconnectOpen}
	title="Remove Stripe credentials?"
	description="Paid event types will stop being bookable."
	confirmText="Remove"
	destructive
	onConfirm={disconnect}
/>
{/if}

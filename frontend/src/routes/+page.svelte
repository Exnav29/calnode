<script lang="ts">
	import { onMount, onDestroy } from 'svelte';
	import { base } from '$app/paths';
	import { api, type Booking, type CalendarStatus, type AvailabilityRule, type EventType, type User } from '$lib/api';
	import { authStatus } from '$lib/stores';
	import { prefs, fmtTime } from '$lib/prefs';
	import { fmtMoney } from '$lib/money';

	type LinkItem = { slug: string; name: string; duration: number; price: string };

	let calendarConnected = $state(false);
	let calendarConfigured = $state(true);
	let hasAvailability = $state(false);
	let hasEventType = $state(false);
	let eventLinks = $state<LinkItem[]>([]);
	let eventNames = $state<Record<string, string>>({});
	let upcoming = $state<Booking[]>([]);
	let upcomingTotal = $state(0);
	let firstName = $state('');
	let myHandle = $state('');
	let origin = $state('');
	let loading = $state(true);
	let copiedSlug = $state<string | null>(null);
	let copyFailed = $state(false);
	let copyTimer: ReturnType<typeof setTimeout> | null = null;
	const now = new Date();

	onMount(async () => {
		origin = window.location.origin;
		try {
			const [cal, rules, events, me, next] = await Promise.all([
				api.get<CalendarStatus>('/v1/calendar/status').catch(() => ({ connected: false, configured: false })),
				api.get<{ items: AvailabilityRule[] }>('/v1/availability-rules').catch(() => ({ items: [] })),
				api.get<{ items: EventType[] }>('/v1/event-types').catch(() => ({ items: [] })),
				api.get<User>('/v1/users/me').catch(() => null),
				api
					.get<{ items: Booking[]; counts: { upcoming: number } }>(
						'/v1/bookings?when=upcoming&order=asc&status=confirmed&limit=4'
					)
					.catch(() => ({ items: [] as Booking[], counts: { upcoming: 0 } }))
			]);
			myHandle = me?.handle ?? '';
			firstName = (me?.name ?? '').trim().split(/\s+/)[0] ?? '';
			calendarConfigured = cal.configured !== false;
			calendarConnected = cal.connected;
			hasAvailability = (rules.items?.length ?? 0) > 0;
			hasEventType = (events.items?.length ?? 0) > 0;
			eventNames = Object.fromEntries((events.items ?? []).map((e) => [e.slug, e.name]));
			// Only bookable events belong in "Booking links": an archived or
			// deactivated event type still answers its /book/ URL but is not being
			// offered, so advertising it here reads as live and invites bookings the
			// host has switched off.
			eventLinks = (events.items ?? [])
				.filter((e) => e.slug && e.is_active && !e.archived)
				.sort((a, b) => a.display_order - b.display_order || a.name.localeCompare(b.name))
				.map((e) => ({
					slug: e.slug,
					name: e.name,
					duration: e.duration_minutes,
					price: e.price_cents > 0 ? fmtMoney(e.price_cents, e.currency) : 'Free'
				}));
			upcoming = next.items ?? [];
			upcomingTotal = next.counts?.upcoming ?? upcoming.length;
		} finally {
			loading = false;
		}
	});

	onDestroy(() => {
		if (copyTimer !== null) clearTimeout(copyTimer);
	});

	const calendarDone = $derived(calendarConnected);
	// Calendar connect is disabled in the demo (see calendar.go's demoMode guard), so the
	// checklist treats it the same as "not configured" — required, forever-incomplete, and
	// pointing at a dead-end connect flow otherwise.
	const calendarRequired = $derived(calendarConfigured && !$authStatus.demo_mode);
	const allDone = $derived((!calendarRequired || calendarDone) && hasAvailability && hasEventType);
	const bookingUrl = (slug: string) => (slug && origin ? `${origin}/book/${slug}` : '');
	const profileUrl = $derived(myHandle && origin ? `${origin}/u/${myHandle}` : '');

	const locale = $derived($prefs.date_format === 'mdy' ? 'en-US' : 'en-GB');
	const greeting = now.getHours() < 12 ? 'Good morning' : now.getHours() < 18 ? 'Good afternoon' : 'Good evening';
	const todayLabel = $derived(now.toLocaleDateString(locale, { weekday: 'long', day: 'numeric', month: 'long' }));

	function sameDay(a: Date, b: Date) {
		return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
	}
	function dayLabel(iso: string) {
		const d = new Date(iso);
		if (sameDay(d, now)) return 'Today';
		const tomorrow = new Date(now);
		tomorrow.setDate(now.getDate() + 1);
		if (sameDay(d, tomorrow)) return 'Tomorrow';
		return d.toLocaleDateString(locale, { weekday: 'short', day: 'numeric', month: 'short' });
	}
	const minutes = (b: Booking) => Math.round((new Date(b.end_at).getTime() - new Date(b.start_at).getTime()) / 60000);
	const isToday = (b: Booking) => sameDay(new Date(b.start_at), now);
	const meetUrl = (b: Booking) => (b.location_value && /^https:\/\//.test(b.location_value) ? b.location_value : '');
	const attendee = (b: Booking) => b.attendees?.[0]?.name || b.attendees?.[0]?.email || 'Guest';

	const summary = $derived.by(() => {
		if (upcoming.length === 0) return 'Nothing booked right now. Share your page and new sessions will appear here.';
		const n = upcomingTotal;
		const first = upcoming[0];
		const when = dayLabel(first.start_at);
		return `${n} upcoming ${n === 1 ? 'session' : 'sessions'}. The next is ${when === 'Today' || when === 'Tomorrow' ? when.toLowerCase() : 'on ' + when} at ${fmtTime(first.start_at, $prefs)}.`;
	});

	async function copyText(text: string, key: string) {
		if (!text) return;
		try {
			await navigator.clipboard.writeText(text);
			copiedSlug = key;
			copyFailed = false;
		} catch {
			copyFailed = true;
			copiedSlug = null;
		}
		if (copyTimer !== null) clearTimeout(copyTimer);
		copyTimer = setTimeout(() => { copiedSlug = null; copyFailed = false; }, 2000);
	}

	function copyLink(slug: string) {
		return copyText(bookingUrl(slug), slug);
	}

	const copyLabel = (key: string, idle = 'Copy') => (copiedSlug === key ? 'Copied' : copyFailed ? 'Failed' : idle);
</script>

<svelte:head><title>Home — Bulletproof Scheduling</title></svelte:head>

{#snippet check(done: boolean, hover = true)}
	<div class="mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full border-2
		{done ? 'border-primary bg-primary' : `border-muted-foreground/40 ${hover ? 'group-hover:border-primary/60' : ''}`}">
		{#if done}
			<svg width="10" height="10" viewBox="0 0 10 10" fill="none" xmlns="http://www.w3.org/2000/svg">
				<path d="M2 5l2.5 2.5L8 3" stroke="white" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
			</svg>
		{/if}
	</div>
{/snippet}

{#snippet chevron()}
	<svg class="mt-0.5 shrink-0 text-muted-foreground/40 group-hover:text-muted-foreground transition-colors" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
		<polyline points="9 18 15 12 9 6"/>
	</svg>
{/snippet}

{#snippet copyButton(key: string, onclick: () => void)}
	<button
		{onclick}
		class="inline-flex h-[30px] shrink-0 items-center rounded-lg border bg-card px-2.5 text-[13px] font-medium transition-colors hover:border-[#cfc6b4]
			{copyFailed ? 'border-destructive text-destructive' : ''}"
	>
		{copyLabel(key)}
	</button>
{/snippet}

{#if loading}
	<p class="py-8 text-sm text-muted-foreground">Loading…</p>
{:else if allDone}
	<p class="eyebrow !text-bronze">{todayLabel}</p>
	<h1 class="mt-2.5 text-[2.5rem] leading-[1.1]">{greeting}{firstName ? `, ${firstName}` : ''}.</h1>
	<p class="mt-3 text-base text-[#3e4a60]">{summary}</p>

	<div class="mt-9 grid items-start gap-7 lg:grid-cols-[minmax(0,7fr)_minmax(0,5fr)]">
		<!-- Up next -->
		<section class="overflow-hidden rounded-[14px] border bg-card">
			<header class="flex items-baseline justify-between px-6 pb-3.5 pt-5">
				<h2 class="eyebrow">Up next</h2>
				<a href="{base}/bookings" class="text-sm font-medium text-primary hover:underline underline-offset-2">All bookings →</a>
			</header>
			{#if upcoming.length === 0}
				<div class="border-t px-6 py-10 text-center">
					<p class="font-display text-xl">A clear calendar.</p>
					<p class="mt-1 text-sm text-muted-foreground">New bookings will show up here as soon as they are made.</p>
				</div>
			{:else}
				<ul class="divide-y border-t">
					{#each upcoming as b (b.id)}
						<li class="grid grid-cols-[6.5rem_minmax(0,1fr)_auto] items-center gap-5 px-6 py-[18px]">
							<div class="tabular-nums leading-tight">
								<p class="text-[12.5px] font-semibold uppercase tracking-[0.08em] {isToday(b) ? 'text-bronze' : 'text-muted-foreground'}">{dayLabel(b.start_at)}</p>
								<p class="mt-0.5 text-[17px] font-semibold">{fmtTime(b.start_at, $prefs)}</p>
								<p class="text-[13px] text-muted-foreground">{minutes(b)} min</p>
							</div>
							<div class="min-w-0">
								<p class="truncate text-base font-semibold">{attendee(b)}</p>
								<p class="truncate text-sm text-muted-foreground">{eventNames[b.event_type_slug] ?? b.event_type_slug}</p>
							</div>
							{#if isToday(b) && meetUrl(b)}
								<a href={meetUrl(b)} target="_blank" rel="noopener noreferrer"
									class="inline-flex h-[30px] items-center rounded-lg bg-primary px-3 text-[13px] font-semibold text-primary-foreground transition-colors hover:bg-foreground">Join</a>
							{:else if b.payment_status === 'paid' && b.amount_paid_cents}
								<span class="inline-flex items-center gap-1.5 text-[13px] font-semibold tabular-nums text-bronze">
									<span class="h-1.5 w-1.5 rounded-full bg-brass" aria-hidden="true"></span>Paid {fmtMoney(b.amount_paid_cents, b.amount_paid_currency)}
								</span>
							{:else if b.payment_status === 'pending'}
								<span class="text-[13px] text-muted-foreground">Payment pending</span>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</section>

		<div class="flex flex-col gap-7">
			{#if profileUrl}
				<!-- Public page -->
				<section class="rounded-[14px] bg-sidebar p-6 text-sidebar-foreground">
					<h2 class="eyebrow !text-brass">Your public page</h2>
					<p class="mt-3 font-display text-[21px] leading-snug">Where clients choose a session and book.</p>
					<p class="mt-3.5 break-all text-sm text-[#b9c3d6]">{profileUrl.replace(/^https?:\/\//, '')}</p>
					<div class="mt-[18px] flex gap-2.5">
						<button
							onclick={() => copyText(profileUrl, 'profile')}
							class="inline-flex h-[34px] items-center rounded-lg bg-white px-3.5 text-sm font-semibold text-foreground transition-colors hover:bg-[#f1ece2]"
						>{copyLabel('profile', 'Copy link')}</button>
						<a href={profileUrl} target="_blank" rel="noopener noreferrer"
							class="inline-flex h-[34px] items-center rounded-lg border border-white/20 px-3.5 text-sm font-medium transition-colors hover:border-white/40">Open ↗</a>
					</div>
				</section>
			{/if}

			{#if eventLinks.length > 0}
				<!-- Booking links -->
				<section class="rounded-[14px] border bg-card px-6 pb-2 pt-5">
					<h2 class="eyebrow mb-1">{eventLinks.length === 1 ? 'Booking link' : 'Booking links'}</h2>
					<ul class="divide-y">
						{#each eventLinks as e (e.slug)}
							<li class="flex items-center justify-between gap-3 py-3">
								<a href={bookingUrl(e.slug)} target="_blank" rel="noopener noreferrer" class="group min-w-0" title={bookingUrl(e.slug)}>
									<p class="truncate text-[15px] font-semibold group-hover:underline underline-offset-2">{e.name}</p>
									<p class="text-[13px] tabular-nums text-muted-foreground">{e.duration} min · {e.price}</p>
								</a>
								{@render copyButton(e.slug, () => copyLink(e.slug))}
							</li>
						{/each}
					</ul>
				</section>
			{/if}
		</div>
	</div>
{:else}
	<p class="eyebrow !text-bronze">Setup</p>
	<h1 class="mt-2.5 text-[2.5rem] leading-[1.1]">Getting started</h1>
	<p class="mt-3 mb-9 text-base text-[#3e4a60]">Complete these steps and you'll be ready to take bookings.</p>

	<!-- Checklist -->
	<div class="overflow-hidden rounded-[14px] border bg-card divide-y">
		<!-- Calendar (hidden in the demo — connect is disabled there) -->
		{#if !$authStatus.demo_mode}
		<a
			href="{base}/{calendarConfigured ? 'calendar' : 'settings/google'}"
			class="flex items-start gap-4 px-6 py-5 transition-colors hover:bg-[#fbf9f5] group"
		>
			{@render check(calendarConnected)}
			<div class="flex-1 min-w-0">
				<p class="text-[15px] font-semibold {calendarConnected ? 'text-muted-foreground line-through' : ''}">
					Connect your calendar
				</p>
				<p class="mt-0.5 text-sm text-muted-foreground">
					{calendarConfigured ? 'Bulletproof Scheduling checks your calendar to prevent double-bookings.' : 'Google Calendar setup required — see the Calendar page for details.'}
				</p>
			</div>
			{@render chevron()}
		</a>
		{/if}

		<!-- Availability -->
		<a
			href="{base}/availability"
			class="flex items-start gap-4 px-6 py-5 transition-colors hover:bg-[#fbf9f5] group"
		>
			{@render check(hasAvailability)}
			<div class="flex-1 min-w-0">
				<p class="text-[15px] font-semibold {hasAvailability ? 'text-muted-foreground line-through' : ''}">
					Set your availability
				</p>
				<p class="mt-0.5 text-sm text-muted-foreground">
					Define the hours when people can book time with you.
				</p>
			</div>
			{@render chevron()}
		</a>

		<!-- Event type -->
		<a
			href="{base}/event-types"
			class="flex items-start gap-4 px-6 py-5 transition-colors hover:bg-[#fbf9f5] group"
		>
			{@render check(hasEventType)}
			<div class="flex-1 min-w-0">
				<p class="text-[15px] font-semibold {hasEventType ? 'text-muted-foreground line-through' : ''}">
					Create your first event type
				</p>
				<p class="mt-0.5 text-sm text-muted-foreground">
					An event type defines the duration, location, and availability for a booking.
				</p>
			</div>
			{@render chevron()}
		</a>

		<!-- Booking link -->
		<div class="flex items-start gap-4 px-6 py-5 {hasEventType ? '' : 'opacity-40'}">
			{@render check(hasEventType, false)}
			<div class="flex-1 min-w-0">
				<p class="text-[15px] font-semibold">Share your booking link</p>
				<p class="mt-0.5 text-sm text-muted-foreground">
					{hasEventType ? 'Your page is live. Share it and start taking bookings.' : "Available once you've created an event type."}
				</p>
				{#if hasEventType && eventLinks.length > 0}
					<div class="mt-2 flex items-center gap-2">
						<a
							href={bookingUrl(eventLinks[0].slug)}
							target="_blank"
							rel="noopener noreferrer"
							class="truncate text-xs text-primary hover:underline font-mono"
						>
							{bookingUrl(eventLinks[0].slug)}
						</a>
						{@render copyButton(eventLinks[0].slug, () => copyLink(eventLinks[0].slug))}
					</div>
				{/if}
			</div>
		</div>
	</div>
{/if}

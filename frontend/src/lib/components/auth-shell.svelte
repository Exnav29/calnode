<script lang="ts">
	import type { Snippet } from 'svelte';

	// Shared frame for the signed-out pages (sign in, claim, invite, password reset):
	// a navy brand panel beside the form. The panel folds away on narrow screens,
	// leaving the wordmark above the form.
	let { children }: { children: Snippet } = $props();
</script>

{#snippet wordmark(size: number, accent: string)}
	<span class="flex items-center gap-3">
		<img src="/admin/brand/bulletproof-icon.png" alt="" width={size} height={size} />
		<span class="flex flex-col leading-[1.15]">
			<span class="text-[13px] font-bold uppercase tracking-[0.16em]">Bulletproof</span>
			<span class="font-display text-sm font-normal italic tracking-normal {accent}">Scheduling</span>
		</span>
	</span>
{/snippet}

<div class="flex min-h-screen bg-background">
	<section class="hidden w-[42%] max-w-[600px] flex-col justify-between bg-sidebar px-16 py-14 text-sidebar-foreground lg:flex">
		{@render wordmark(32, 'text-brass')}
		<div>
			<div class="mb-7 h-px w-10 bg-brass"></div>
			<p class="font-display text-[44px] font-normal leading-[1.12] tracking-[-0.015em]">Your private<br />booking desk.</p>
			<p class="mt-5 max-w-[380px] text-[16.5px] leading-relaxed text-[#b9c3d6]">
				Sessions, availability and payments for Bulletproof Automations, in one quiet place.
			</p>
		</div>
		<p class="text-[13px] text-[#8e9ab1]">Bulletproof Automations</p>
	</section>
	<section class="flex flex-1 flex-col items-center justify-center p-6 sm:p-12">
		<div class="mb-10 text-foreground lg:hidden"><!-- Bronze, not brass: brass on sand is under 3:1. -->
			{@render wordmark(30, 'text-bronze')}</div>
		<div class="w-full max-w-[380px]">
			{@render children()}
		</div>
	</section>
</div>

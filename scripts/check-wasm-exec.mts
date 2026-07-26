#!/usr/bin/env -S npx tsx
/**
 * Guards the vendored copy of Go's wasm_exec.js.
 *
 * The package ships wasm_exec.js so a consumer does not have to find, copy and
 * keep it in sync themselves. That only works if the copy stays honest: a Go
 * toolchain upgrade changes the file, and a mismatched wasm_exec.js against a
 * newer Go binary fails in ways that look like anything but a version skew.
 *
 * Our copy differs from the toolchain's by exactly one patch — two lines that
 * record the exit code, which Go's own copy discards. This checks that the two
 * files are otherwise identical, and fails loudly rather than shipping a
 * silently stale runtime.
 */
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

const VENDORED = new URL('../src/wasm_exec.js', import.meta.url)

/** The lines we add, which must be the only difference. */
const PATCH = ['\t\t\tthis.exitCode = undefined', '\t\t\t\tthis.exitCode = code']

function goroot(): string {
	return execFileSync('go', ['env', 'GOROOT'], { encoding: 'utf8' }).trim()
}

function main(): void {
	const upstreamPath = join(goroot(), 'lib', 'wasm', 'wasm_exec.js')
	const upstream = readFileSync(upstreamPath, 'utf8')
	const vendored = readFileSync(VENDORED, 'utf8')

	const remaining = [...PATCH]
	const stripped = vendored
		.split('\n')
		.filter((line) => {
			const i = remaining.indexOf(line)
			if (i === -1) return true
			remaining.splice(i, 1)
			return false
		})
		.join('\n')

	if (remaining.length > 0) {
		fail(
			`src/wasm_exec.js is missing the exitCode patch:\n  ${remaining.join('\n  ')}\n\n` +
				'Re-apply it, or update PATCH in this script if the shape changed.',
		)
	}

	if (stripped !== upstream) {
		const a = upstream.split('\n')
		const b = stripped.split('\n')
		const at = a.findIndex((line, i) => line !== b[i])
		fail(
			`src/wasm_exec.js does not match ${upstreamPath}.\n\n` +
				`First difference at line ${at + 1}:\n` +
				`  toolchain: ${JSON.stringify(a[at])}\n` +
				`  vendored:  ${JSON.stringify(b[at])}\n\n` +
				'The Go toolchain changed wasm_exec.js. Copy the new one over src/wasm_exec.js,\n' +
				'reapply the two exitCode lines, and rerun.',
		)
	}

	console.log(`wasm_exec.js matches ${upstreamPath} (plus the exitCode patch)`)
}

function fail(message: string): never {
	console.error(`\nwasm_exec.js drift check failed.\n\n${message}\n`)
	process.exit(1)
}

main()

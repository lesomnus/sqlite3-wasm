#!/usr/bin/env -S npx tsx
/**
 * Proves the packaging claim the way only a real consumer can.
 *
 * The in-repo tests import `dist/` in place, which is not the same thing as
 * being installed: `npm pack` decides what actually ships, and the consumer's
 * own bundler — not ours — has to resolve the exports map, follow the lazy
 * chunk, and keep the side-effect imports. Every packaging bug found so far was
 * invisible until something outside the repo tried to use the output.
 *
 * What this does:
 *   1. builds and `npm pack`s the package
 *   2. creates a throwaway Vite app that installs the tarball
 *   3. builds that app with its own Vite
 *   4. serves the result with COOP/COEP
 *   5. loads it in Chromium and waits for the Go program to report its exit code
 *
 * Run:  npx tsx scripts/check-downstream.mts
 */
import { createServer } from 'node:http'
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync, cpSync } from 'node:fs'
import { extname, join, resolve } from 'node:path'
import { execFileSync } from 'node:child_process'
import { chromium } from 'playwright-core'

const repo = resolve(import.meta.dirname, '..')
const work = join(repo, 'node_modules', '.downstream')
const app = join(work, 'app')

function run(cmd: string, args: string[], cwd: string): void {
	execFileSync(cmd, args, { cwd, stdio: 'inherit' })
}

function scaffold(tarball: string): void {
	mkdirSync(join(app, 'src'), { recursive: true })

	// A Go program to run. The driver example asserts its way through the type
	// system, transactions and the error model, so a zero exit code here means
	// the installed package works end to end.
	const wasm = join(repo, 'src', 'examples', 'driver.wasm')
	if (!existsSync(wasm)) {
		throw new Error('src/examples/driver.wasm is missing — run `npm run build:examples` first')
	}
	cpSync(wasm, join(app, 'src', 'app.wasm'))

	writeFileSync(
		join(app, 'package.json'),
		JSON.stringify(
			{
				name: 'downstream-consumer',
				private: true,
				type: 'module',
				scripts: { build: 'vite build' },
				dependencies: { 'sqlite3-wasm-go': `file:${tarball}` },
				devDependencies: { vite: readVersion('vite') },
			},
			null,
			2,
		) + '\n',
	)

	// Deliberately plain: no plugin, no alias, no optimizeDeps entry. If the
	// package needs any of that, it has not met its own requirement.
	writeFileSync(
		join(app, 'vite.config.ts'),
		`import { defineConfig } from 'vite'\nexport default defineConfig({ build: { target: 'esnext' } })\n`,
	)

	writeFileSync(
		join(app, 'index.html'),
		`<!doctype html><html><body><script type="module" src="/src/main.ts"></script></body></html>\n`,
	)

	writeFileSync(
		join(app, 'src', 'main.ts'),
		`import { runGoWasm } from 'sqlite3-wasm-go/go-worker'
import appWasm from './app.wasm?url'

declare global {
	interface Window { __result?: { ok: boolean; detail: string } }
}

async function main() {
	const { worker, exited } = await runGoWasm(appWasm)
	try {
		const code = await exited
		window.__result = { ok: code === 0, detail: 'exit code ' + code }
	} catch (e) {
		window.__result = { ok: false, detail: String(e) }
	} finally {
		worker.terminate()
	}
}

main()
`,
	)
}

function readVersion(pkg: string): string {
	const json = JSON.parse(readFileSync(join(repo, 'package.json'), 'utf8'))
	const v = json.devDependencies?.[pkg] ?? json.dependencies?.[pkg]
	if (!v) throw new Error(`cannot find ${pkg} in the repository's package.json`)
	return v
}

const MIME: Record<string, string> = {
	'.html': 'text/html',
	'.js': 'text/javascript',
	'.mjs': 'text/javascript',
	'.wasm': 'application/wasm',
	'.map': 'application/json',
}

async function serve(root: string): Promise<{ url: string; close(): Promise<void> }> {
	const server = createServer((req, res) => {
		const path = decodeURIComponent((req.url ?? '/').split('?')[0])
		const file = join(root, path === '/' ? 'index.html' : path)
		// Cross-origin isolation, without which the opfs VFS silently does not
		// install and SharedArrayBuffer is absent.
		res.setHeader('Cross-Origin-Opener-Policy', 'same-origin')
		res.setHeader('Cross-Origin-Embedder-Policy', 'require-corp')
		if (!existsSync(file)) {
			res.statusCode = 404
			res.end('not found')
			return
		}
		res.setHeader('Content-Type', MIME[extname(file)] ?? 'application/octet-stream')
		res.end(readFileSync(file))
	})

	await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
	const address = server.address()
	if (typeof address === 'string' || address === null) throw new Error('no address')
	return {
		url: `http://127.0.0.1:${address.port}/`,
		close: () => new Promise<void>((r) => server.close(() => r())),
	}
}

async function main(): Promise<void> {
	console.log('building and packing…')
	run('npm', ['run', 'build'], repo)

	rmSync(work, { recursive: true, force: true })
	mkdirSync(work, { recursive: true })
	const packed = execFileSync('npm', ['pack', '--pack-destination', work], {
		cwd: repo,
		encoding: 'utf8',
	})
	// npm pack prints the filename last.
	const tarball = join(work, packed.trim().split('\n').pop() as string)

	console.log('scaffolding a consumer app…')
	scaffold(tarball)

	console.log('installing the tarball…')
	run('npm', ['install', '--no-audit', '--no-fund', '--silent'], app)

	console.log('building the consumer app with its own Vite…')
	run('npx', ['vite', 'build'], app)

	const site = await serve(join(app, 'dist'))
	const browser = await chromium.launch()
	try {
		const page = await browser.newPage()
		const errors: string[] = []
		page.on('pageerror', (e) => errors.push(String(e)))
		await page.goto(site.url)

		const result = await page.waitForFunction(() => window.__result, undefined, {
			timeout: 60_000,
		})
		const value = (await result.jsonValue()) as { ok: boolean; detail: string }

		if (!value.ok) {
			throw new Error(`the consumer app failed: ${value.detail}\n${errors.join('\n')}`)
		}
		console.log(`\ndownstream consumer OK (${value.detail})`)
	} finally {
		await browser.close()
		await site.close()
	}
}

main().catch((e) => {
	console.error(`\ndownstream check failed:\n${e?.stack ?? e}\n`)
	process.exit(1)
})

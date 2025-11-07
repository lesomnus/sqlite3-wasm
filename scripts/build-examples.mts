#!/usr/bin/env -S npx tsx
import "zx/globals";

import { existsSync, promises as fs } from 'node:fs'


export const rootDir = path.dirname(import.meta.dirname);
cd(rootDir)

$.env.GOOS = 'js'
$.env.GOARCH = 'wasm'

async function buildExample(examplePath: string) {
    const pkgName = path.basename(path.dirname(examplePath));
    const outFile = path.join(rootDir, 'src/examples', `${pkgName}.wasm`);
    const outDir = path.dirname(outFile);
    
    await $`mkdir -p ${outDir}`;
    await $`go build -o ${outFile} ${examplePath}`;
    
    console.log(`Built ${pkgName} -> ${outFile}`);
}

async function main() {
    const dir = path.join(rootDir, 'examples');
    const pkgs = await fs.readdir(dir, { withFileTypes: true });
	for(const pkg of pkgs) {
		const p = path.join(dir, pkg.name, 'main.go')
		if(!existsSync(p)){
			continue
		}

		await buildExample(p)
	}
}

main().catch(console.error);

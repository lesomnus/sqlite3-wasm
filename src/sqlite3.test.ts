import { describe, test } from "vitest";

import { sqlite3Worker1Promiser, type Promiser } from '@sqlite.org/sqlite-wasm'

describe('sqlite3', ()=>{
	test('query', async ()=>{
		const driver = await sqlite3Worker1Promiser.v2()
		await driver('open', {filename: 'file::memory:'})

		await exec(driver, `SELECT 1 + 1 AS result;`)

		await exec(driver, `CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  age INTEGER
);
		`)
		await exec(driver, `INSERT INTO users (name, age) VALUES ('Alice', 30), ('Bob', 25);`)
		await exec(driver, `SELECT * FROM users;`)
	})
})

async function exec(driver: Promiser, query: string) {
	let vs: any[] = []
	const {result} = await driver('exec', {
		sql: query,
		callback: (c)=>{vs.push(c)}
	})
	console.log(vs)
	console.log(result)
}

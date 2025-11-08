import { sqlite3Worker1Promiser } from '@sqlite.org/sqlite-wasm';

(globalThis as any)['sqlite-wasm-go'] = () => sqlite3Worker1Promiser.v2();

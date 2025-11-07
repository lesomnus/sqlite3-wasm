type Go = {
	argv: string[];
	env: { [envKey: string]: string };
	exit: (code: number) => void;
	importObject: WebAssembly.Imports;
	exited: boolean;
	exitCode: number | undefined;
	mem: DataView;
	run(instance: WebAssembly.Instance): Promise<void>;
};

export declare global {
	var Go: new () => Go;
}

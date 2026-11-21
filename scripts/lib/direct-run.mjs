import { realpathSync } from "node:fs";
import { fileURLToPath } from "node:url";

export function runsAsMain(url, argv1 = process.argv[1]) {
	if (!argv1) return false;
	const real = (p) => {
		try {
			return realpathSync(p);
		} catch {
			return null;
		}
	};
	const self = real(fileURLToPath(url));
	const invoked = real(argv1);
	return self === null || invoked === null || self === invoked;
}

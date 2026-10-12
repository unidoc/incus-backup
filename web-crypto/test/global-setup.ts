// Builds the wasm module and generates the test repository with the real Go engine. Both FAIL the
// run when they fail: there is no fallback fixture.
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = fileURLToPath(new URL('..', import.meta.url));

export default function setup(): void {
  execFileSync('sh', ['scripts/build-wasm.sh'], { cwd: here, stdio: 'inherit' });
  execFileSync('sh', ['scripts/fixture.sh', '.cache/fixture'], { cwd: here, stdio: 'inherit' });
}

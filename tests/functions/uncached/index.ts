// A function whose module is never warmed in the tests: with CACHED_ONLY=1 it must fail at once instead of downloading.
import isOdd from 'npm:is-odd@3.0.1';
Deno.serve(() => new Response(String(isOdd(3))));

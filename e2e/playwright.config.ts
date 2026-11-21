import { defineConfig, devices } from '@playwright/test';

// One spec, Chromium (the engine the design measured Sec-Fetch-Dest in).
// forbidOnly and retries 0: a flaky gate must show as red, not pass on retry.
export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  forbidOnly: true,
  retries: 0,
  timeout: 120_000, // the first `go run` of the harness compiles it
  reporter: [['list']],
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});

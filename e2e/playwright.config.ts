import { defineConfig, devices } from '@playwright/test';

// Chromium (the engine the design measured Sec-Fetch-Dest in). The project's
// viewport is a desktop's; a spec about another size sets its own
// (lobby-mobile.spec.ts: 390x844; the ui-*.spec.ts run both sizes).
// forbidOnly and retries 0: a flaky gate must show as red, not pass on retry.
//
// Every page gets Chromium's fake camera and microphone, granted without a
// prompt (the pre-join's device check), and audio that may start without a
// gesture (its level meter).
const use = {
  ...devices['Desktop Chrome'],
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'] },
};

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  forbidOnly: true,
  retries: 0,
  timeout: 120_000, // the first `go run` of the harness compiles it
  reporter: [['list']],
  expect: { toHaveScreenshot: { maxDiffPixelRatio: 0.002, animations: 'disabled', caret: 'hide' } },
  snapshotPathTemplate: '{testDir}/goldens/{testFilePath}/{arg}{ext}',
  projects: [{
    name: 'chromium',
    testIgnore: 'ui-visual.spec.ts',
    use,
  }, {
    // The goldens: only in the pinned Playwright image (CI's `visual` job,
    // scripts/update-goldens.sh), where the fonts and the rasteriser are
    // fixed. `npx playwright test --project=visual`.
    name: 'visual',
    testMatch: 'ui-visual.spec.ts',
    use,
  }],
});

// @testing-library/jest-dom v7 splits its vitest matcher augmentation into
// the /vitest entrypoint — the bare import only augments jest/chai, so
// importing it here keeps expect().toBeInTheDocument() typed AND runtime-
// registered under vitest 5 (setupFiles runs before every test file).
import '@testing-library/jest-dom/vitest';

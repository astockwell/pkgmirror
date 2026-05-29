// tailwind.config.js
//
// Drives content scanning for the Tailwind standalone CLI. Only the
// console package's templates are scanned — the existing internal/ui
// uses Bootstrap and isn't touched.
//
// See tools/tailwindcss/install.sh for how to install the standalone
// CLI binary used by `make build-css` / `make watch-css`.

module.exports = {
  content: [
    './internal/console/pages/**/*.tmpl',
    './internal/console/partials/**/*.tmpl',
    './internal/console/layouts/**/*.tmpl',
  ],
  darkMode: 'class',
  theme: {
    extend: {},
  },
};

const sonarjs = require('eslint-plugin-sonarjs');

// Match the existing Sonar production JavaScript scope. These four rules
// cover the findings that escaped PR validation; this is not full Sonar parity.
module.exports = [
  {
    ignores: [
      '**/node_modules/**', '**/coverage/**', '**/.github/**', '**/docs/**',
      'tests/desktop/**', 'clients/gnome/tests/**',
      '**/*.test.js', '**/*.test.mjs', '**/*.test.cjs',
    ],
  },
  {
    files: ['**/*.{js,mjs,cjs}'],
    plugins: { sonarjs },
    linterOptions: { noInlineConfig: true },
    rules: {
      'sonarjs/no-nested-conditional': 'error',
      'sonarjs/no-nested-template-literals': 'error',
      'sonarjs/no-nested-functions': ['error', { threshold: 4 }],
      'sonarjs/no-unenclosed-multiline-block': 'error',
    },
  },
];

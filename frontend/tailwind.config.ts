import type { Config } from 'tailwindcss';

/**
 * The palette is semantic rather than decorative.
 *
 * `pass` / `escalate` / `block` map one-to-one onto domain.PolicyResult, and
 * `recovered` / `at-risk` onto the two states an operator scans a queue for. A
 * reviewer reading this console is making money decisions from colour before they
 * read the label, so a colour that means two different things on two screens is a
 * correctness problem, not a style one.
 */
const config: Config = {
  content: ['./src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        ink: {
          900: '#10111a',
          800: '#171925',
          700: '#202331',
          600: '#2b2f40',
          500: '#393e52',
        },
        line: {
          DEFAULT: '#303447',
          strong: '#454a60',
        },
        body: '#e7ecf6',
        muted: '#93a0b8',
        dim: '#5f6c86',
        accent: {
          DEFAULT: '#67c8d0',
          soft: '#17343a',
          text: '#b9eef1',
        },
        pass: {
          DEFAULT: '#34d399',
          soft: '#0f2f28',
        },
        escalate: {
          DEFAULT: '#fbbf24',
          soft: '#33260a',
        },
        block: {
          DEFAULT: '#fb7185',
          soft: '#3a1220',
        },
        recovered: {
          DEFAULT: '#4ade80',
          soft: '#12301f',
        },
      },
      fontFamily: {
        sans: [
          'IBM Plex Sans',
          'Aptos',
          'Aptos Display',
          'Segoe UI Variable',
          'Segoe UI',
          'ui-sans-serif',
          'system-ui',
          'Arial',
          'sans-serif',
        ],
        // Amounts, identifiers and idempotency keys are tabular data. A
        // proportional font makes two amounts of the same magnitude different
        // widths, which is exactly when a reader misreads one for the other.
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'Liberation Mono', 'monospace'],
        display: ['Georgia', 'Times New Roman', 'serif'],
      },
      fontSize: {
        '2xs': ['0.6875rem', { lineHeight: '1rem' }],
      },
      borderRadius: {
        card: '0.625rem',
      },
      boxShadow: {
        card: '0 1px 0 0 rgba(255,255,255,0.035) inset, 0 16px 32px -24px rgba(0,0,0,0.7)',
      },
      keyframes: {
        shimmer: {
          '0%': { opacity: '0.45' },
          '50%': { opacity: '0.8' },
          '100%': { opacity: '0.45' },
        },
      },
      animation: {
        shimmer: 'shimmer 1.4s ease-in-out infinite',
      },
    },
  },
  plugins: [],
};

export default config;

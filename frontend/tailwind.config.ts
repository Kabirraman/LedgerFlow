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
          900: '#080b12',
          800: '#0d1220',
          700: '#141b2d',
          600: '#1c2438',
          500: '#273148',
        },
        line: {
          DEFAULT: '#242e44',
          strong: '#33405c',
        },
        body: '#e7ecf6',
        muted: '#93a0b8',
        dim: '#5f6c86',
        accent: {
          DEFAULT: '#5b8cff',
          soft: '#1e2a4d',
          text: '#a8c1ff',
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
          'ui-sans-serif',
          'system-ui',
          '-apple-system',
          'Segoe UI',
          'Roboto',
          'Helvetica Neue',
          'Arial',
          'sans-serif',
        ],
        // Amounts, identifiers and idempotency keys are tabular data. A
        // proportional font makes two amounts of the same magnitude different
        // widths, which is exactly when a reader misreads one for the other.
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'Liberation Mono', 'monospace'],
      },
      fontSize: {
        '2xs': ['0.6875rem', { lineHeight: '1rem' }],
      },
      borderRadius: {
        card: '0.75rem',
      },
      boxShadow: {
        card: '0 1px 0 0 rgba(255,255,255,0.03) inset, 0 8px 24px -12px rgba(0,0,0,0.6)',
      },
      keyframes: {
        'fade-in': {
          from: { opacity: '0', transform: 'translateY(2px)' },
          to: { opacity: '1', transform: 'translateY(0)' },
        },
        shimmer: {
          '0%': { opacity: '0.45' },
          '50%': { opacity: '0.8' },
          '100%': { opacity: '0.45' },
        },
      },
      animation: {
        'fade-in': 'fade-in 160ms ease-out',
        shimmer: 'shimmer 1.4s ease-in-out infinite',
      },
    },
  },
  plugins: [],
};

export default config;

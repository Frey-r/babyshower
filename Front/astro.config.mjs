import { defineConfig } from 'astro/config';

export default defineConfig({
  output: 'static',
  srcDir: './src',
  publicDir: './public',
  build: { format: 'directory' },
});

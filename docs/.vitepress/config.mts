import { defineConfig } from 'vitepress'

// https://vitepress.dev/reference/site-config
export default defineConfig({
  title: 'Burrow',
  description:
    'Self-hosted reverse tunnels — expose a local port through a relay you run yourself.',
  base: '/', // custom domain (docs.burrow.com); use '/burrow/' if served from a project page
  cleanUrls: true,
  lastUpdated: true,
  markdown: {
    // `env` is not a built-in Shiki grammar; alias it to `properties` so
    // KEY=value blocks highlight cleanly instead of falling back to plain text.
    languageAlias: { env: 'properties' },
  },
  themeConfig: {
    nav: [
      { text: 'Guide', link: '/guide/introduction' },
      { text: 'Reference', link: '/reference/cli' },
    ],
    sidebar: {
      '/guide/': [
        {
          text: 'Guide',
          items: [
            { text: 'Introduction', link: '/guide/introduction' },
            { text: 'Quickstart', link: '/guide/quickstart' },
            { text: 'Deploy on a server', link: '/guide/deploy' },
            { text: 'Connect a client', link: '/guide/connect-client' },
            { text: 'Expose services', link: '/guide/expose-services' },
            { text: 'Access control & security', link: '/guide/access-control' },
            { text: 'Configuration', link: '/guide/configuration' },
            { text: 'Operations', link: '/guide/operations' },
            { text: 'Troubleshooting', link: '/guide/troubleshooting' },
          ],
        },
      ],
      '/reference/': [
        {
          text: 'Reference',
          items: [
            { text: 'CLI', link: '/reference/cli' },
            { text: 'HTTP API', link: '/reference/api' },
          ],
        },
      ],
    },
    search: { provider: 'local' },
    socialLinks: [
      { icon: 'github', link: 'https://github.com/ankoehn/burrow' },
    ],
    footer: {
      message: 'Released under the Apache-2.0 License.',
      copyright: 'Copyright © Burrow contributors',
    },
  },
})

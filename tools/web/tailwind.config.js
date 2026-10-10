/** @type {import('tailwindcss').Config} */
module.exports = {
    content: ['../../web/index.html', '../../web/app.js'],
    theme: {
        extend: {
            fontFamily: {
                sans: ['"Plus Jakarta Sans Variable"', '"Plus Jakarta Sans"', 'system-ui', 'sans-serif'],
                display: ['"Outfit Variable"', 'Outfit', 'system-ui', 'sans-serif'],
            },
        },
    },
    plugins: [],
};

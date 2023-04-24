import { parseArgs } from "util";
import { config } from "dotenv";

config();

const {
    values: { url, key },
} = parseArgs({
    options: {
        url: {
            type: "string",
            short: "u",
        },
        key: {
            type: "string",
            short: "k",
        },
    },
});

const apiKey = key || process.env.OPENAI_API_KEY;

if (!apiKey) {
    throw new Error("No API key provided");
} else if (!url) {
    throw new Error("No URL provided");
}

const srcUrl = url;

export { apiKey, srcUrl };
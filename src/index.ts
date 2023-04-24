import { inMemory } from "node-inmemory";
import { OpenAIEmbeddings } from "langchain/embeddings/openai";
import { apiKey, srcUrl } from "./args";
import { crawl } from "./crawlers";

const [getEmbeddings] = inMemory(() => new OpenAIEmbeddings({
    openAIApiKey: apiKey,
}));

async function getEmbeddingsFromUrl(url: string) {
    const embeddings = getEmbeddings();
    const docs = await crawl(url);
    const texts = docs.map(doc => doc.pageContent);
    return await embeddings.embedDocuments(texts);
}

(async () => {
    const embeddings = await getEmbeddingsFromUrl(srcUrl);
    console.log(embeddings);
})();
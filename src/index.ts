import { OpenAI } from "langchain/llms/openai";
import { apiKey, srcUrl } from "./args";
import { crawl } from "./crawlers";
import { VectorDBQAChain } from "langchain/chains";
import { getVectorStore, saveVector } from "./store";

async function getEmbeddingsFromUrl(url: string) {
    const docs = await crawl(url);
    const texts = docs.map(doc => doc.pageContent);
    await saveVector(texts);
    const model = new OpenAI({
        openAIApiKey: apiKey,
    });
    const chain = VectorDBQAChain.fromLLM(model, getVectorStore(), {
        returnSourceDocuments: true,
    });
    return await chain.call({query: "什么是QOS?"});
}

(async () => {
    const embeddings = await getEmbeddingsFromUrl(srcUrl);
    console.log(embeddings);
})();
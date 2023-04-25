import inquirer from "inquirer";
import ora from "ora";
import { Store } from "../store";

export abstract class Handler {
    constructor(protected store: Store) { }

    protected abstract handle(): Promise<void>;

    async start() {
        await this.handle();
        const history = [];

        while (true) {
            const { question } = await inquirer.prompt([
                {
                    type: 'input',
                    name: 'question',
                    message: 'Enter question: ',
                    validate: async (input: string) => {
                        if (input.length === 0) {
                            return 'Please enter a question';
                        }
                        return true;
                    }
                },
            ]);

            const spinner = ora("Thinking...").start();
            const chain = await this.store.getChain();
            const { text } = await chain.call({ question, chat_history: history });
            history.push(question, text);
            spinner.stop();

            console.log(text);
        }
    }
}

import WebHandler from "./web";
export { WebHandler };